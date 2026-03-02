---
name: dm-responder
description: Draft context-aware replies to direct messages. Read conversation history to understand context before responding.
---

When you receive a direct message:

1. Read the last 10 messages in the conversation for context (use get_space_history)
2. Identify the sender and their intent
3. Draft a reply that matches the tone and formality of the conversation
4. Present the draft to the user for approval before sending
5. If auto_respond is enabled, send the draft directly

Keep replies concise and professional. If the message requires information you don't have, flag it to the user rather than guessing.
