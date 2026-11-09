# Journal voice v17: checklists

The Journal-only voice contract accepts `checklistItem` and `completedChecklistItem` in block styles and whole-paragraph format commands. Existing source authorization, exact whole-item anchors, instruction partitioning, and conflict validation apply unchanged.

New actionable items can be emitted as checklist blocks. The prompt requires explicit completion evidence before generating a completed item; plans, negation and conditions are not completion evidence. Existing-item changes use format commands rather than rewriting the item text. Ambiguous targets require clarification.

The client previews both single-item and multi-item checklist commands before confirmation. A proposed format context may therefore have an empty `additionalBlockIDs` array. Confirmation still requires `canConfirm`, a known receipt, and recorded instruction evidence. This does not authorize the server to bypass client stale-target checks.

Local verification: `go test ./internal/journal/...` passes, including checklist source authorization, single-item confirmation, stale confirmation rejection, model schema coverage and whole-paragraph anchor validation.

Deployment is not part of this change. The shared Health routes and deployment configuration are unchanged. Client consent version alignment and real-provider/UI acceptance must be completed separately before claiming end-to-end availability.
