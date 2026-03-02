---
description: Interact with your Webex workspace — list spaces, read messages, send replies, and manage real-time listener.
argument-hint: <subcommand> [args]
allowed-tools:
  - mcp
---

# /webex

Webex workspace integration for Claude Code.

## Subcommands

### Spaces & Messages
- `/webex spaces` — List your Webex spaces
- `/webex read <space>` — Read recent messages from a space
- `/webex send <space> <message>` — Send a message to a space
- `/webex search <query>` — Search messages across spaces

### People
- `/webex people <space>` — List members of a space
- `/webex profile <person>` — Look up a person's profile

### Real-time Listener (v0.2)
- `/webex connect` — Start WebSocket listener for real-time messages
- `/webex disconnect` — Stop WebSocket listener
- `/webex status` — Show connection state and notification buffer
- `/webex notifications` — Show buffered inbound messages

### Intelligence (v0.3)
- `/webex meetings` — List upcoming meetings
- `/webex transcript <meeting>` — Get meeting transcript
- `/webex digest [space] [hours]` — Activity digest across spaces
- `/webex context <query>` — Cross-space topic search with context

### Configuration
- `/webex config` — Show current settings and agent routing

## Usage

When no subcommand is given, show a summary of your Webex activity:
recent spaces, unread counts, and listener status.

Use the Webex MCP tools to fulfill requests. Match user intent to the appropriate tool:
- "list my spaces" → `list_spaces`
- "what's new in #ops" → `get_space_history`
- "send X to Y" → `send_message`
- "reply to that thread" → `reply_to_thread`
- "who is in #engineering" → `get_users`
- "search for deployment" → `search_messages`
