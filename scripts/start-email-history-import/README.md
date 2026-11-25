# Start the mailbox history copy

This program starts the one-time copy of the existing mailbox history (#5481).
Run the **Start Email History Import** workflow from reviewed `main`, first
attempt, with confirmation `start-email-history-import`. It runs through
`prod-operation.yaml`, so it shares the production deploy lock and refuses any
other invocation before it reads the cluster.

It creates one Job from the suspended `private-mail/email-history-import`
CronJob and returns without waiting. It refuses, creating nothing, when:

- the CronJob is missing, unreadable or no longer suspended;
- the `email-history-import` ExternalSecret has not synced;
- an earlier copy is still running or has not finished.

It prints only `EMAIL_HISTORY_IMPORT_START=STARTED job=<name>`, a fixed
`REFUSED reason=<reason>` (exit 2), or `UNKNOWN` when the create call failed
(exit 3). The copy's own aggregate receipt is in the Job's log; see
[the runbook](../../docs/operations/email-history-import.md).
