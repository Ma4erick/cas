# ── Config ────────────────────────────────────────────────────────────────────
REGISTRY        ?= localhost:5001        # port-forward to Zot inside k3d
REGISTRY_INTERNAL ?= registry.local:5000 # how k3s/FluxCD sees the registry
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

## Push Docker image to Zot
push-image: build
	docker push $(REGISTRY)/$(IMAGE_NAME):$(VERSION)
	docker push $(REGISTRY)/$(IMAGE_NAME):latest

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
