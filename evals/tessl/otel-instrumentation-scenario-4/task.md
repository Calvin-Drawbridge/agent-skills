# JavaScript SDK upgrade and trace validation

## Problem description

The TEST-checkout service and its browser client are upgrading to OpenTelemetry JavaScript SDK 2.x.
Build the setup and trace tests for SDK 2.x without downgrading the SDK.

The service uses a custom span processor to remove sensitive request headers, followed by an OTLP export processor.
The browser uses a `WebTracerProvider` and a batch export processor.
Keep the processor order and use valid ESM imports in both setups.
Use `https://telemetry.example.test/v1/traces` as the browser’s illustrative endpoint, and keep credentials out of the source.

The service sits behind a trusted gateway that propagates W3C trace context.
Additional sampling is managed in the Collector.
Validate requests with no incoming parent, a sampled remote parent, and an unsampled remote parent.
Explain the expected trace shape and sampling behavior in the tests.

Use an in-memory exporter to test span hygiene for outbound calls and missing parents, including positive and negative cases.
Tests capturing only this service may receive an explicitly supplied remote parent context whose span is outside the capture.
Account for the scope of that capture in the tests.
Use real SDK spans and flush the provider before inspecting finished spans.

## Output specification

Produce the following files:

1. `instrumentation.mjs` — SDK 2.x Node tracer setup, including the custom processor before the export processor.
2. `browser-instrumentation.mjs` — SDK 2.x browser tracer setup.
3. `trace-validation.test.mjs` — runnable tests for all parentage, sampling, and hygiene cases above, using an in-memory exporter and no external telemetry backend.
4. `package.json` — compatible OpenTelemetry dependencies and a command to run the tests.

Keep fixture data synthetic, using `TEST-` identifiers and reserved example domains.
Hexadecimal trace and span IDs must be valid, non-zero values.
