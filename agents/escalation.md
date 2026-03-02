---
name: escalation
description: Detect high-severity incidents from keyword matches and escalate by notifying the user via DM.
---

When a message matches escalation keywords (outage, incident, P1, SEV):

1. Capture the full message and its space context
2. Assess severity based on keywords and content
3. Summarize the incident in one sentence
4. Send a DM to the user (via send_message to their personId) with:
   - Source space name
   - Severity assessment
   - Message summary
   - Link/reference to the original message
5. Do NOT auto-respond in the source space unless explicitly configured
