---
name: webex-monitor
description: Automatically checks for new Webex notifications when the real-time listener is active. Peeks at the notification buffer (non-destructive) and routes messages to the appropriate agents.
version: 0.2.0
---

This skill activates when the Webex WebSocket listener is connected.

When active:
1. Call `get_notifications` to peek at the inbound message buffer (reading does not remove messages or clear reminders)
2. If there are new messages, check each against the agent routing config (`get_notification_routes`)
3. For each matched route, follow the instructions in the corresponding agent definition
4. For critical-priority messages, surface them immediately to the user
5. For lower-priority messages, batch them for the next digest cycle
6. Once a message has actually been handled, call `mark_processed` to clear it from the pending list (`get_pending`)

This skill is part of v0.2 and requires the WebSocket listener to be running (`/webex connect`).
