# Cross-cluster blue/green replication scenario

This scenario validates an AWS-style blue/green MariaDB upgrade across two
Kubernetes clusters without changing operator code.

- Blue runs in the Blue Kubernetes cluster with `mariadb:10.6`.
- Green runs in the Green Kubernetes cluster. It restores the Blue physical
  backup with `mariadb:10.6`, then rolls forward to `mariadb:10.11`.
- Cross-cluster replication uses externally reachable database endpoints, not
  `*.svc.cluster.local` DNS.
- Cutover is manual: put Blue into maintenance/read-only mode, promote Green,
  and then update the application connection string to the Green endpoint.

## Required replacements

Before applying the manifests, replace these placeholder hosts with endpoints
that are reachable from both Kubernetes clusters:

```text
blue-db.example.test     -> Blue primary endpoint
green-db.example.test    -> Green primary endpoint
s3.example.test:9000     -> Shared S3-compatible backup endpoint
```

The database endpoints can be cloud LoadBalancers, MetalLB IPs with DNS names,
VPN-routed addresses, or any other endpoint that both clusters can reach. The
S3 endpoint must also be reachable from both clusters because Blue writes the
backup and Green restores from it.

The MariaDB and MinIO secret names intentionally match the existing examples:

```text
mariadb
minio
minio-ca
mariadb-server-ca
```

Create equivalent secrets in both Kubernetes clusters. All MariaDB clusters in
the topology must share the same MariaDB credentials and TLS CA.

## Apply

Use two kubeconfig contexts:

```sh
export BLUE_CONTEXT=blue-cluster
export GREEN_CONTEXT=green-cluster
```

Apply the shared prerequisites in both clusters:

```sh
kubectl --context "$BLUE_CONTEXT" apply -f examples/manifests/config/mariadb-secret.yaml
kubectl --context "$BLUE_CONTEXT" apply -f examples/manifests/config/minio-secret.yaml
kubectl --context "$GREEN_CONTEXT" apply -f examples/manifests/config/mariadb-secret.yaml
kubectl --context "$GREEN_CONTEXT" apply -f examples/manifests/config/minio-secret.yaml
```

Create Blue:

```sh
kubectl --context "$BLUE_CONTEXT" apply -f examples/manifests/multi-cluster/replication-blue-green-cross-cluster/blue-cluster.yaml
kubectl --context "$BLUE_CONTEXT" wait --for=condition=Ready mariadb/mariadb-blue --timeout=10m
```

Create the Blue physical backup in shared S3 storage:

```sh
kubectl --context "$BLUE_CONTEXT" apply -f examples/manifests/multi-cluster/replication-blue-green-cross-cluster/blue-backup.yaml
kubectl --context "$BLUE_CONTEXT" wait --for=condition=Complete physicalbackup/physicalbackup-blue --timeout=10m
```

Bootstrap Green from the Blue physical backup:

```sh
kubectl --context "$GREEN_CONTEXT" apply -f examples/manifests/multi-cluster/replication-blue-green-cross-cluster/green-cluster-bootstrap.yaml
kubectl --context "$GREEN_CONTEXT" wait --for=condition=Ready mariadb/mariadb-green --timeout=10m
```

Roll Green forward to MariaDB `10.11`:

```sh
kubectl --context "$GREEN_CONTEXT" apply -f examples/manifests/multi-cluster/replication-blue-green-cross-cluster/green-cluster-upgrade-10.11.yaml
kubectl --context "$GREEN_CONTEXT" wait --for=condition=Ready mariadb/mariadb-green --timeout=10m
```

The two-step Green bootstrap is intentional. In same-cluster validation,
preparing a `mariadb:10.6` physical backup directly with `mariadb-backup` from
`mariadb:10.11` failed with:

```text
InnoDB: File ./ib_logfile0 is too small
mariadb-backup: srv_start() returned 11 (Generic error)
```

Green also sets `spec.replication.gtidStrictMode: false`. With strict mode
enabled, the second Green pod can fail internal replication with error 1236
because both Green pods are restored from the Blue backup and the restored GTID
position is not available in the Green primary binlog.

## Validate

Get the MariaDB password from either cluster:

```sh
export MARIADB_ROOT_PASSWORD="$(
  kubectl --context "$BLUE_CONTEXT" get secret mariadb -o jsonpath='{.data.password}' | base64 -d
)"
```

Write through Blue:

```sh
kubectl --context "$BLUE_CONTEXT" exec -it mariadb-blue-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
CREATE DATABASE IF NOT EXISTS bgtest;
CREATE TABLE IF NOT EXISTS bgtest.events (id INT PRIMARY KEY, note VARCHAR(64));
INSERT INTO bgtest.events VALUES (1, "from-blue")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
SELECT @@version;
SELECT * FROM bgtest.events;
'
```

Read through Green:

```sh
kubectl --context "$GREEN_CONTEXT" exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
SELECT @@version;
SELECT * FROM bgtest.events;
'
```

Confirm Green is running `10.11.x` after the upgrade:

```sh
kubectl --context "$GREEN_CONTEXT" exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e 'SELECT @@version;'
kubectl --context "$GREEN_CONTEXT" exec -it mariadb-green-1 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e 'SELECT @@version;'
```

Write another row through Blue and confirm it appears on Green:

```sh
kubectl --context "$BLUE_CONTEXT" exec -it mariadb-blue-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
INSERT INTO bgtest.events VALUES (2, "replicated-cross-cluster")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
'
kubectl --context "$GREEN_CONTEXT" exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
SELECT * FROM bgtest.events ORDER BY id;
'
```

Check Green replication status:

```sh
kubectl --context "$GREEN_CONTEXT" get mariadb mariadb-green -o jsonpath='{.status.replication}' | jq
```

Success requires:

- `slaveIORunning=true`
- `slaveSQLRunning=true`
- `secondsBehindMaster=0`

## Cutover

Put Blue into maintenance/read-only mode:

```sh
kubectl --context "$BLUE_CONTEXT" patch mariadb mariadb-blue --type merge -p '{
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
```

Wait until Green replication lag is zero:

```sh
kubectl --context "$GREEN_CONTEXT" get mariadb mariadb-green -o jsonpath='{.status.replication}' | jq
```

Promote Green in the Green cluster:

```sh
kubectl --context "$GREEN_CONTEXT" patch mariadb mariadb-green --type merge \
  -p '{"spec":{"multiCluster":{"primary":"mariadb-green"}}}'
kubectl --context "$GREEN_CONTEXT" wait --for=condition=Ready mariadb/mariadb-green --timeout=10m
```

Demote Blue in the Blue cluster so it follows Green:

```sh
kubectl --context "$BLUE_CONTEXT" patch mariadb mariadb-blue --type merge \
  -p '{"spec":{"multiCluster":{"primary":"mariadb-green"}}}'
```

Verify Green accepts writes:

```sh
kubectl --context "$GREEN_CONTEXT" exec -it mariadb-green-0 -- mariadb -uroot -p"$MARIADB_ROOT_PASSWORD" -e '
INSERT INTO bgtest.events VALUES (3, "written-after-cross-cluster-cutover")
  ON DUPLICATE KEY UPDATE note = VALUES(note);
SELECT * FROM bgtest.events ORDER BY id;
'
```

After this succeeds, manually update application traffic from the Blue external
endpoint to the Green external endpoint. The operator does not update external
application load balancers.

## Scope

This example documents the cross-cluster version of the tested same-cluster
blue/green scenario. It uses the same operator behavior and the same two-step
physical restore workaround. It does not add operator implementation code and
does not test rollback replication from Green `10.11` back to Blue `10.6`.
