# Blue/Green replication scenario

This scenario validates an AWS-style blue/green database upgrade without changing
operator code and without adding automated tests.

- Blue runs `mariadb:10.6` and starts as the writable production endpoint.
- Green bootstraps the physical backup with `mariadb:10.6`, then rolls forward
  to `mariadb:10.11` and keeps replicating from Blue.
- Database users can connect to both `mariadb-blue-primary` and
  `mariadb-green-primary` before cutover.
- Application traffic is moved manually by changing the application connection
  string from Blue to Green.

## Apply

Create the shared credentials and object storage prerequisites used by the
existing multi-cluster examples first, then apply the scenario in this order:

```sh
kubectl apply -f examples/manifests/config/mariadb-secret.yaml
kubectl apply -f examples/manifests/config/minio-secret.yaml
export MARIADB_ROOT_PASSWORD="$(kubectl get secret mariadb -o jsonpath='{.data.password}' | base64 -d)"
kubectl apply -f examples/manifests/multi-cluster/replication-blue-green/blue.yaml
kubectl wait --for=condition=Ready mariadb/mariadb-blue --timeout=10m
kubectl apply -f examples/manifests/multi-cluster/replication-blue-green/blue-backup.yaml
kubectl wait --for=condition=Complete physicalbackup/physicalbackup-blue --timeout=10m
kubectl apply -f examples/manifests/multi-cluster/replication-blue-green/green.yaml
kubectl wait --for=condition=Ready mariadb/mariadb-green --timeout=10m
kubectl apply -f examples/manifests/multi-cluster/replication-blue-green/green-upgrade-10.11.yaml
kubectl wait --for=condition=Ready mariadb/mariadb-green --timeout=10m
```

The S3 backup settings intentionally match the existing multi-cluster examples.
If the environment uses a different object store, update the `s3` fields in
`blue-backup.yaml`, `green.yaml`, and `green-upgrade-10.11.yaml`.

Do not bootstrap Green directly with `mariadb:10.11` for this scenario. In the
kind-based validation, `mariadb-backup --prepare` from `mariadb:10.11` failed
against the `mariadb:10.6` physical backup with:

```text
InnoDB: File ./ib_logfile0 is too small
mariadb-backup: srv_start() returned 11 (Generic error)
```

The tested workaround is:

1. Restore the Blue physical backup into Green using `mariadb:10.6`.
2. Set Green `spec.replication.gtidStrictMode: false`.
3. Wait until Green is ready and lag is zero.
4. Apply `green-upgrade-10.11.yaml` to roll Green forward to `mariadb:10.11`.

The `gtidStrictMode: false` setting is needed for this scenario because both
Green pods are restored from the Blue backup. With strict mode enabled, the
second Green pod can fail internal replication with error 1236 because the
restored GTID position is not available in the Green primary binlog.

## Endpoints

Use both endpoints before cutover:

```text
mariadb://root:<password>@mariadb-blue-primary.default.svc.cluster.local:3306
mariadb://root:<password>@mariadb-green-primary.default.svc.cluster.local:3306
```

With the sample LoadBalancer IPs:

```text
mariadb://root:<password>@172.18.1.20:3306
mariadb://root:<password>@172.18.1.25:3306
```

## Validate

Write test data through Blue:

```sh
kubectl exec -it mariadb-blue-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
CREATE DATABASE IF NOT EXISTS bgtest;
CREATE TABLE IF NOT EXISTS bgtest.events (id INT PRIMARY KEY, note VARCHAR(64));
INSERT INTO bgtest.events VALUES (1, "from-blue")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
SELECT @@version;
SELECT * FROM bgtest.events;
'
```

Read the data through Green:

```sh
kubectl exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
SELECT @@version;
SELECT * FROM bgtest.events;
'
```

After `green-upgrade-10.11.yaml` is applied, both Green pods should report
`10.11.x`:

```sh
kubectl exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e 'SELECT @@version;'
kubectl exec -it mariadb-green-1 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e 'SELECT @@version;'
```

Write another row through Blue and confirm it appears on Green:

```sh
kubectl exec -it mariadb-blue-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
INSERT INTO bgtest.events VALUES (2, "replicated-after-bootstrap")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
'
kubectl exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
SELECT * FROM bgtest.events ORDER BY id;
'
```

Check replication status on the Green `MariaDB` resource:

```sh
kubectl get mariadb mariadb-green -o jsonpath='{.status.replication}' | jq
```

Success requires the Green replica status to report:

- `slaveIORunning=true`
- `slaveSQLRunning=true`
- `secondsBehindMaster=0`

## Cutover

Before promotion, put Blue into maintenance/read-only mode and wait for Green
replication lag to reach zero:

```sh
kubectl patch mariadb mariadb-blue --type merge -p '{
  "spec": {
    "maintenance": {
      "enabled": true,
      "cordon": true,
      "drainConnections": true,
      "drainGracePeriodSeconds": 30,
      "readOnly": true
    }
  }
}'
kubectl get mariadb mariadb-green -o jsonpath='{.status.replication}' | jq
```

Promote Green:

```sh
kubectl patch mariadb mariadb-green --type merge \
  -p '{"spec":{"multiCluster":{"primary":"mariadb-green"}}}'
```

Demote Blue so it follows the new primary:

```sh
kubectl patch mariadb mariadb-blue --type merge \
  -p '{"spec":{"multiCluster":{"primary":"mariadb-green"}}}'
```

Verify Green accepts writes:

```sh
kubectl exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
INSERT INTO bgtest.events VALUES (3, "written-after-cutover")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
SELECT * FROM bgtest.events ORDER BY id;
'
```

After this succeeds, manually update the application connection string from the
Blue endpoint to the Green endpoint. The operator does not move application
traffic automatically in this scenario.

## Scope

This is a one-way upgrade rehearsal from MariaDB `10.6` to `10.11`. It does not
test reverse replication or rollback from Green to Blue. If physical restore
between these versions exposes compatibility issues, record the scenario as a
risk and repeat with a logical backup or a backup image compatible with `10.6`.
