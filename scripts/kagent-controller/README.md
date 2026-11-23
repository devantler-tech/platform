# kagent controller startup backport

The deployed kagent 1.0.0 scheduler can give up while Substrate is still
restoring a cold worker. A rejected start can then keep its one dispatch claim
and the worker indefinitely. The [startup incident](https://github.com/devantler-tech/platform/issues/6463)
records the production evidence.

`scheduled-startup.patch` applies to kagent commit
`a08cde76ad6482dd758a4220b14213d79185ac30`. It gives cold startup time to
finish within the router budget. If no task was accepted, the scheduler
releases the empty runtime through the session lifecycle with a matching
execution lease. The session row lock fences late task saves. A live dispatch
reservation or any task or checkpoint history blocks cleanup. The scheduler
keeps the claim when a response is uncertain and recovers accepted work from
history. Accepted tasks keep their configured execution deadline.

The publisher tests the patch, replaces the controller binary in the pinned
upstream runtime image, and signs the verified image digest from `main`.
The Platform Helm release uses a reviewed digest. Return to the upstream
distribution once a supported release handles cold startup, safe recovery of
unused worker assignments, and rejected dispatches. [kagent scheduler tracking](https://github.com/kagent-dev/kagent/issues/3727)
