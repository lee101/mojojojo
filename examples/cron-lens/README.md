# Cron Lens

Paste a cron expression and see the next twelve times it will actually run—in
your chosen time zone. It uses the adjacent
[`mojo-croniter`](https://github.com/lee101/mojo-croniter) bulk calendar kernel
when available and falls back to `croniter` on a cold worker.

[![Deploy to mojojojo](https://mojojojo.cc/static/deploy-button.svg)](https://mojojojo.cc/studio?example=cron-lens&kind=tool)

This is deliberately an answer, not a cron-expression generator: time zones,
DST transitions, weekday ranges, and month boundaries are where schedules get
surprising. The server returns ISO timestamps and the browser formats them for
humans. License: MIT.
