---
name: cas
description: Share current work context with the team by creating a CAS session with a summary of what has been done so far
allowed-tools: Bash(curl *)
argument-hint: "[session name]"
---

Share the current conversation context with the team via CAS (Collaborative Agent Sessions).

The CAS server runs at http://localhost:8080.

Steps:
1. Create a new CAS session named "$ARGUMENTS" (if no name given, derive a short descriptive name from the work done so far)
2. Write a clear, structured summary of the work done in this conversation so far — what problem was being solved, what was changed or built, key decisions made, and what's next. Format it as a useful handoff note for teammates.
3. Post that summary as the first message in the new CAS session using the session ID from step 1.
4. Report the session name and the URL: http://localhost:8080

Use these API calls:

Create session:
```
curl -s -X POST http://localhost:8080/api/sessions \
  -H "Content-Type: application/json" \
  -d '{"name": "<session name>"}'
```

Post message (replace SESSION_ID with the id field from the response above, and escape the summary as valid JSON):
```
curl -s -X POST http://localhost:8080/api/sessions/SESSION_ID/messages \
  -H "Content-Type: application/json" \
  -d '{"content": "<summary>", "sender": "Claude Code"}'
```
