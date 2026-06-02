---
name: cas-pull
description: Pull a CAS session's context and working directory into this local Claude Code instance
allowed-tools: Bash(curl *) Bash(cd *) Bash(ls *)
argument-hint: "[session name]"
---

Pull context from a CAS session into this Claude Code instance.

The CAS server runs at http://localhost:8080.

Steps:

1. Fetch the list of sessions from CAS:
```
curl -s http://localhost:8080/api/sessions
```

2. Find the session matching "$ARGUMENTS" (case-insensitive name match). If no argument was given, list all available sessions and ask the user which one to pull.

3. Fetch the full session history by reading the `.cas/session.json` file in the session's working directory, OR by connecting to the session WebSocket. Use the `workDir` field from the session to locate the project.

4. Read the session messages and build a structured summary of:
   - What was discussed and worked on in the session
   - Any files that were edited or created
   - Key decisions made
   - Current state of the work
   - What was left to do or in progress

5. Report the session's `workDir` so the user knows where the project lives locally. Suggest they run:
   ```
   cd <workDir>
   ```
   to set their working directory to the project.

6. Present the full context summary clearly so this Claude Code instance is fully briefed on the CAS session's work and can continue seamlessly from where the team left off.

If CAS is not running at localhost:8080, say so clearly and remind the user to start it with `go run .` in the cas-go directory.
