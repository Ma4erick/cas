---
name: cas-pull
description: Pull a CAS session's context and working directory into this local Claude Code instance
allowed-tools: Bash(curl *) Bash(ls *) Bash(cat *)
argument-hint: "[session name]"
---

Pull context from a CAS session into this Claude Code instance.

The CAS server runs at http://localhost:8080.

Steps:

1. Read CAS credentials from `~/.cas.env`. Look for `CAS_USERNAME` and `CAS_PASSWORD` lines:
```
grep -E '^CAS_USERNAME=|^CAS_PASSWORD=' ~/.cas.env
```
If either is missing, ask the user for their CAS username and password before continuing.

Authenticate with CAS using those credentials:
```
curl -s -c /tmp/cas-cookies.txt -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"$CAS_USERNAME\",\"password\":\"$CAS_PASSWORD\"}"
```
If the response contains `"error"`, ask the user to check their credentials in `~/.cas.env` and retry.

2. Fetch the list of sessions using the auth cookie:
```
curl -s -b /tmp/cas-cookies.txt http://localhost:8080/api/sessions
```

3. Find the session matching "$ARGUMENTS" (case-insensitive name match). If no argument was given, list all available sessions and ask the user which one to pull.

4. Fetch the session's full message history from the database. The session ID is in the `id` field of the session list. Use psql if available to query messages directly:
```
psql "postgresql://london.summers@localhost:5432/cas?sslmode=disable" -c \
  "SELECT role, sender, content, created_at FROM messages WHERE session_id = '<id>' ORDER BY created_at ASC;"
```
If psql is not available, read the `.cas/session.json` file in the session's `workDir`.

5. Read the session messages and build a structured summary of:
   - What was discussed and worked on in the session
   - Any files that were edited or created
   - Key decisions made
   - Current state of the work
   - What was left to do or in progress

6. List the top-level contents of the session's `workDir` to understand the project structure.

7. Report the session's `workDir` and suggest:
   ```
   cd <workDir>
   ```

8. Present the full context clearly so this Claude Code instance is fully briefed and can continue seamlessly.

If CAS is not running at localhost:8080, say so clearly and remind the user to start it with `go run .` in the cas-go directory.

Clean up the cookie file when done:
```
rm -f /tmp/cas-cookies.txt
```
