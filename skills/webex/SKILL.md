---
name: webex
description: Interact with Cisco Webex — list spaces, read/send messages, search, people, meetings, transcripts, adaptive cards — all via direct REST API calls. No MCP server required.
version: 1.0.0
---

# Webex Skill

Standalone Webex integration for Claude Code. Uses the Webex REST API directly via `curl`. Requires only `WEBEX_TOKEN` set in the environment.

## Prerequisites

The environment variable `WEBEX_TOKEN` must contain a valid Webex access token.
- Personal Access Token (12h): https://developer.webex.com/docs/getting-your-personal-access-token
- Or an OAuth integration token (long-lived, auto-refresh via the MCP server)

Before executing any API call, verify the token is set:
```bash
[ -z "$WEBEX_TOKEN" ] && echo "ERROR: WEBEX_TOKEN not set" && exit 1
```

## API Base

All endpoints use `https://webexapis.com/v1`. Always include:
```
-H "Authorization: Bearer $WEBEX_TOKEN"
-H "Content-Type: application/json"
```

## Capabilities

### List Spaces
Show the user's Webex spaces sorted by recent activity.
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/rooms?sortBy=lastactivity&max=50" | jq '.items[] | {id, title, type, lastActivity}'
```

### Read Space Messages
Read recent messages from a space. Requires `room_id`.
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/messages?roomId=ROOM_ID&max=20" | jq '.items[] | {personEmail, text, created}'
```

### Send Message
Send to a space (roomId), person by ID (toPersonId), or person by email (toPersonEmail). Provide exactly one destination.
```bash
# To a space:
curl -s -X POST -H "Authorization: Bearer $WEBEX_TOKEN" -H "Content-Type: application/json" \
  -d '{"roomId":"ROOM_ID","text":"MESSAGE"}' \
  "https://webexapis.com/v1/messages" | jq '{id, created}'

# To a person by email:
curl -s -X POST -H "Authorization: Bearer $WEBEX_TOKEN" -H "Content-Type: application/json" \
  -d '{"toPersonEmail":"EMAIL","text":"MESSAGE"}' \
  "https://webexapis.com/v1/messages" | jq '{id, created}'
```

### Reply to Thread
Reply to a specific message thread in a space. Requires `roomId` and `parentId`.
```bash
curl -s -X POST -H "Authorization: Bearer $WEBEX_TOKEN" -H "Content-Type: application/json" \
  -d '{"roomId":"ROOM_ID","parentId":"PARENT_MSG_ID","text":"REPLY"}' \
  "https://webexapis.com/v1/messages" | jq '{id, created}'
```

### Search Messages
The Webex API has no server-side search. To search, fetch messages from a space and filter client-side:
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/messages?roomId=ROOM_ID&max=200" | jq --arg q "QUERY" '.items[] | select(.text | test($q; "i")) | {personEmail, text, created}'
```
To search across spaces: list spaces first, then search each space individually. Cap to 10 spaces max.

### List Space Members
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/memberships?roomId=ROOM_ID&max=100" | jq '.items[] | {personDisplayName, personEmail, isModerator}'
```

### Get User Profile
Use `me` for the authenticated user, or a person ID.
```bash
# Self:
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/people/me" | jq '{displayName, emails, status, title}'

# By ID:
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/people/PERSON_ID" | jq '{displayName, emails, status, title}'
```

### Send Adaptive Card
Rich card with tables, buttons, inputs. The `text` field is fallback for clients that don't render cards.
```bash
curl -s -X POST -H "Authorization: Bearer $WEBEX_TOKEN" -H "Content-Type: application/json" \
  -d '{
    "roomId": "ROOM_ID",
    "text": "Adaptive Card (fallback)",
    "attachments": [{
      "contentType": "application/vnd.microsoft.card.adaptive",
      "content": CARD_JSON_OBJECT
    }]
  }' \
  "https://webexapis.com/v1/messages" | jq '{id, created}'
```

### List Meetings
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/meetings?from=FROM_ISO8601&to=TO_ISO8601&max=20" | jq '.items[] | {id, title, start, end, state, hostDisplayName}'
```
Default: from=now, to=7 days from now. Dates in ISO 8601 (e.g., `2026-03-25T00:00:00Z`).

### Get Meeting Transcript
Two-step: list transcripts for a meeting, then download.
```bash
# Step 1: List transcripts for the meeting
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/meetingTranscripts?meetingId=MEETING_ID" | jq '.items[] | {id, status, startTime}'

# Step 2: Download transcript (txt or vtt)
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/meetingTranscripts/TRANSCRIPT_ID/download?format=txt"
```

### Add Reaction
React to a message with an emoji.
```bash
curl -s -X POST -H "Authorization: Bearer $WEBEX_TOKEN" -H "Content-Type: application/json" \
  -d '{"parentId":"MESSAGE_ID","text":"EMOJI"}' \
  "https://webexapis.com/v1/messages" | jq '{id, created}'
```

## Interaction Patterns

When the user says:
- "list my spaces" / "show spaces" → List Spaces
- "read #ops" / "what's happening in X" → Find space by name in List Spaces, then Read Space Messages
- "send X to Y" / "tell Y about X" → Send Message (resolve Y to room_id or email)
- "reply to that" / "respond in thread" → Reply to Thread (need parent message ID)
- "who is in #engineering" → List Space Members
- "who am I" / "my profile" → Get User Profile (me)
- "search for deployment" → Search Messages
- "my meetings" / "what meetings do I have" → List Meetings
- "transcript for X" → Get Meeting Transcript
- "send a card to X" → Send Adaptive Card

## Finding Spaces by Name

Users refer to spaces by name, not ID. Always resolve name to ID first:
```bash
curl -s -H "Authorization: Bearer $WEBEX_TOKEN" \
  "https://webexapis.com/v1/rooms?sortBy=lastactivity&max=200" | jq --arg name "SPACE_NAME" '.items[] | select(.title | test($name; "i")) | {id, title}'
```
If multiple matches, show them and ask the user to pick.

## Error Handling

- **401**: Token expired or invalid. Tell the user to refresh their token.
- **429**: Rate limited. Wait and retry once after 5 seconds.
- **404**: Resource not found. Verify the ID is correct.
- Always check `curl` exit code and HTTP status before processing results.

## Security

- Never log or display the full token value. If debugging, show only the last 4 characters.
- Never send the token to any host other than `webexapis.com`.
- Validate message text length ≤ 7439 characters (Webex API limit).
- Validate card JSON ≤ 28000 bytes.
- Always use `jq` to parse JSON — never eval API responses.
