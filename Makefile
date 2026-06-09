# ── Config ────────────────────────────────────────────────────────────────────
REGISTRY ?= 127.0.0.1:5001
REGISTRY_INTERNAL ?= registry.local:5000
IMAGE_NAME      := cas
CHART_DIR       := helm/cas
CHART_NAME      := cas

# Derive version from git tag, fall back to 0.0.0-dev
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "0.0.0-dev")
# Helm chart version must be valid semver — strip 'v' prefix if present
CHART_VERSION   := $(shell echo "$(VERSION)" | sed 's/^v//')

.PHONY: all build push-image package-chart push-chart push deploy version

## Show current version
version:
	@echo "Version: $(VERSION)"
	@echo "Chart:   $(CHART_VERSION)"

## Build everything and push to Zot
all: push-image push-chart
	@echo ""
	@echo "✓ CAS $(VERSION) pushed to Zot"
	@echo "  Image: $(REGISTRY)/$(IMAGE_NAME):$(VERSION)"
	@echo "  Chart: oci://$(REGISTRY)/helm/$(CHART_NAME):$(CHART_VERSION)"

## Build Docker image
build:
	docker build -t $(REGISTRY)/$(IMAGE_NAME):$(VERSION) .
	docker tag $(REGISTRY)/$(IMAGE_NAME):$(VERSION) $(REGISTRY)/$(IMAGE_NAME):latest

## Push Docker image to Zot via skopeo (works with Rancher Desktop)
push-image: build
	docker save $(REGISTRY)/$(IMAGE_NAME):$(VERSION) | \
	  skopeo copy --dest-tls-verify=false docker-archive:/dev/stdin docker://$(REGISTRY)/$(IMAGE_NAME):$(VERSION)
	docker save $(REGISTRY)/$(IMAGE_NAME):latest | \
	  skopeo copy --dest-tls-verify=false docker-archive:/dev/stdin docker://$(REGISTRY)/$(IMAGE_NAME):latest

## Package Helm chart with current version
package-chart:
	@# Patch chart version and appVersion before packaging
	sed -i.bak \
	  -e 's/^version:.*/version: $(CHART_VERSION)/' \
	  -e 's/^appVersion:.*/appVersion: "$(VERSION)"/' \
	  $(CHART_DIR)/Chart.yaml && rm -f $(CHART_DIR)/Chart.yaml.bak
	helm package $(CHART_DIR) --destination /tmp/helm-packages

## Push Helm chart to Zot as OCI artifact
push-chart: package-chart
	helm push /tmp/helm-packages/$(CHART_NAME)-$(CHART_VERSION).tgz \
	  oci://$(REGISTRY)/helm

## Full deploy cycle — build, push image + chart
push: push-image push-chart

## Build, push image + chart, then redeploy in k3d
deploy: push
	@echo ""
	@# Bump chart version and push
	helm package $(CHART_DIR) --version $(CHART_VERSION) --destination /tmp/helm-packages
	helm push /tmp/helm-packages/$(CHART_NAME)-$(CHART_VERSION).tgz oci://$(REGISTRY)/helm --plain-http
	@echo ""
	@# Trigger FluxCD to pick up new chart, then restart pod for new image
	kubectl -n flux-system annotate helmchart.source.toolkit.fluxcd.io cas-cas \
	  reconcile.fluxcd.io/requestedAt="$(shell date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite 2>/dev/null || true
	kubectl -n cas delete pod -l app.kubernetes.io/name=cas 2>/dev/null || true
	@echo ""
	@echo "Waiting for CAS to come up..."
	kubectl -n cas rollout status deployment/cas-cas --timeout=120s 2>/dev/null || \
	  kubectl -n cas wait pod -l app.kubernetes.io/name=cas --for=condition=Ready --timeout=120s
	@echo ""
	@echo "✓ CAS $(VERSION) deployed"

## Push new image only and restart pod (no chart change needed)
restart: push-image
	kubectl -n cas delete pod -l app.kubernetes.io/name=cas
	kubectl -n cas wait pod -l app.kubernetes.io/name=cas --for=condition=Ready --timeout=120s
	@echo "✓ CAS restarted with new image"

## Show what's in Zot
list:
	@echo "=== Images ==="
	@curl -s http://$(REGISTRY)/v2/_catalog | python3 -m json.tool 2>/dev/null || true
	@echo ""
	@echo "=== Helm charts ==="
	@curl -s http://$(REGISTRY)/v2/helm/$(CHART_NAME)/tags/list | python3 -m json.tool 2>/dev/null || true

## Start port-forward to Zot (run in background)
pf:
	kubectl -n zot port-forward svc/zot 5001:5000 &
	@echo "Port-forward active: localhost:5001 → Zot in k3d"

## Stop port-forward
pf-stop:
	@pkill -f "port-forward svc/zot" || true

## Port-forward CAS on all interfaces (localhost + LAN)
## Access via http://localhost:8080 or http://<your-lan-ip>:8080
cas-pf:
	kubectl -n cas port-forward svc/cas-cas 8080:80 --address 0.0.0.0

## Port-forward CAS on localhost only
cas-pf-local:
	kubectl -n cas port-forward svc/cas-cas 8080:80
