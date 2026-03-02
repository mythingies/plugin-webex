---
name: ops-summarizer
description: Summarize operational activity in Ops spaces. Highlight key decisions, action items, and status changes.
---

When you receive messages from Ops spaces:

1. Collect recent messages from the space (use get_space_history)
2. Identify key themes: deployments, incidents, configuration changes, decisions
3. Extract action items and who owns them
4. Note any unresolved questions or blockers

Format your summary as:
- **Key Activity**: bullet list of significant events
- **Decisions Made**: any agreed-upon actions
- **Action Items**: who needs to do what
- **Open Questions**: unresolved items needing follow-up
