---
name: cas
description: Share current work context with the team by creating a CAS session with a summary of what has been done so far
allowed-tools: Bash(curl *) Bash(grep *)
argument-hint: "[session name]"
---

Share the current conversation context with the team via CAS (Collaborative Agent Sessions).

The CAS server runs at http://localhost:8080.

Steps:

1. Read CAS credentials from `~/.cas.env`:
```
grep -E '^CAS_USERNAME=|^CAS_PASSWORD=' ~/.cas.env
```
If either is missing, ask the user for their CAS username and password.

2. Authenticate with CAS:
```
curl -s -c /tmp/cas-cookies.txt -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"$CAS_USERNAME\",\"password\":\"$CAS_PASSWORD\"}"
```
If the response contains `"error"`, report the problem and stop.

3. Create a new CAS session named "$ARGUMENTS" (if no name given, derive a short descriptive name from the work done so far):
```
curl -s -b /tmp/cas-cookies.txt -X POST http://localhost:8080/api/sessions \
  -H "Content-Type: application/json" \
  -d '{"name": "<session name>"}'
```

4. Write a clear, structured summary of the work done in this conversation so far — what problem was being solved, what was changed or built, key decisions made, and what's next. Format it as a useful handoff note for teammates.

5. Post that summary as the first message in the new CAS session (replace SESSION_ID with the id from step 3):
```
curl -s -b /tmp/cas-cookies.txt -X POST http://localhost:8080/api/sessions/SESSION_ID/messages \
  -H "Content-Type: application/json" \
  -d '{"content": "<summary>", "sender": "Claude Code"}'
```

6. Report the session name and the URL: http://localhost:8080

7. Clean up:
```
rm -f /tmp/cas-cookies.txt
```
