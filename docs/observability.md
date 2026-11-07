# Tracing

fuse can export OpenTelemetry traces of `fuse shell`, one-shot `fuse <task>` and
`research-probe` runs over OTLP. Tracing is **off** when the `observability:` block is absent
or `traces.enabled` is false: an empty config yields a no-op observer.

```yaml
observability:
  instance_id: "local"          # empty: $FUSE_INSTANCE_ID, then the hostname
  traces:
    enabled: true
    endpoint: "127.0.0.1:4317"  # OTLP receiver
    protocol: grpc              # or http/protobuf
    insecure: true              # local only
    sample_ratio: 1.0
    headers: {}
```

The block is honored only from the trusted `~/.fuse/config.yml`; a repo's `.fuse.local.yml`
cannot point your telemetry anywhere.

Two behaviors are deliberate:

- **An invalid block fails startup fast**, with a precise error, rather than silently
  degrading to no tracing.
- **An exporter outage never holds up the run.** Spans are exported from a bounded background
  queue and dropped when it is full.

In the shell, the OTEL SDK's own diagnostics go to a log file rather than the terminal, because
an async export error written to stderr would shear the alternate screen.
