# Hammer: A load testing tool for serverless logs

This hammer sets up read and (optionally) write traffic to a log to test correctness and performance under load.

If write traffic is enabled, then the target log must support `POST` requests to a `/add` path.

## Usage

As an example for testing the serving capabilities of the Armored Witness CI log:

```bash
SERVERLESS_LOG_PUBLIC_KEY=transparency.dev-aw-ftlog-ci-2+f77c6276+AZXqiaARpwF4MoNOxx46kuiIRjrML0PDTm+c7BLaAMt6 go run ./hammer -v=2 \
  --log_url=https://api.transparency.dev/armored-witness-firmware/ci/log/2/ \
  --origin="transparency.dev/armored-witness/firmware_transparency/ci/2"
```

This will start a text-based UI in the terminal that shows the current status, logs, and supports increasing/decreasing read and write traffic.
The process can be killed with `<Ctrl-C>`.
