# PBM Exporter

Prometheus exporter for Percona Backup for MongoDB (PBM).

## Metrics

- `pbm_backup_total` — total number of backups
- `pbm_backup_last_timestamp_seconds` — last backup finish time
- `pbm_backup_last_size_bytes` — last backup size
- `pbm_agent_status` — 1 if agent is up, 0 if not
- `pbm_exporter_errors_total` — exporter errors

## Usage

...

## Building

```bash
GOOS=linux GOARCH=amd64 go build -o pbm_exporter