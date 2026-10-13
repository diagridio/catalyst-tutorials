# Deploy

A persistent copy of this demo runs on `demo-production-catalyst`, namespace
`verifiable-execution-demo`, against the Catalyst project `verifiable-execution-demo` in the
Demo Production Catalyst org. There is no CI. The image is built and pushed by hand.

| Piece | Where |
|---|---|
| Agent (`deployment.yaml`) | Deployment `invoice-approver`: one run per pod start, then a worker |
| Workflow store (`postgres.yaml`) | StatefulSet `postgres`, 8Gi `managed-csi` disk, TLS on |
| Store's public address | `diagrid-ve-demo-pg.westeurope.cloudapp.azure.com:5432`, Catalyst's egress address only |
| Catalyst | project `verifiable-execution-demo`, component `wf-store`, agent `invoice-approver` |

Catalyst's sidecar runs in the Catalyst region, not in this cluster, which is why the database
has a public LoadBalancer. See [Who can reach the database](#who-can-reach-the-database).

## Build and push

Nodes are **arm64**. `--provenance=false` is required or containerd rejects the pull.

```bash
aws ecr-public get-login-password --region us-east-1 \
  | docker login --username AWS --password-stdin public.ecr.aws
```

First time only:

```bash
aws ecr-public create-repository --region us-east-1 \
  --repository-name verifiable-execution-demo/invoice-approver
```

```bash
docker buildx build --platform linux/arm64 --provenance=false \
  -t public.ecr.aws/d3f9w4q8/verifiable-execution-demo/invoice-approver:<tag> --push .
```

## Deploy

Run everything from `verifiable-execution/invoice-approver-go/`.

### 1. Postgres

Generate the password and a self-signed certificate. Nothing is printed and nothing is
written to disk outside a temp directory.

```bash
# kubectl scoped to this demo. A function, because zsh would not split a variable into arguments.
kve() { kubectl --context demo-production-catalyst -n verifiable-execution-demo "$@"; }
kubectl --context demo-production-catalyst create namespace verifiable-execution-demo
kve create secret generic postgres-auth --from-literal=password="$(openssl rand -hex 24)"

TMP=$(mktemp -d)
openssl req -new -x509 -days 3650 -nodes -subj /CN=diagrid-ve-demo-pg \
  -keyout "$TMP/tls.key" -out "$TMP/tls.crt" 2>/dev/null
kve create secret tls postgres-tls --cert="$TMP/tls.crt" --key="$TMP/tls.key"
rm -rf "$TMP"
```

```bash
kubectl --context demo-production-catalyst apply -f deploy/postgres.yaml
kve rollout status statefulset/postgres
kve get svc postgres          # wait for EXTERNAL-IP
```

### 2. Catalyst project, store and agent

History signing can only be set when the project is created.

```bash
diagrid project create verifiable-execution-demo --enable-workflow-history-signing \
  --deploy-managed-kv --deploy-managed-pubsub --wait

diagrid component create wf-store --type state.postgresql --metadata actorStateStore=true \
  --project verifiable-execution-demo \
  --metadata connectionString="host=diagrid-ve-demo-pg.westeurope.cloudapp.azure.com port=5432 user=postgres password=$(kve get secret postgres-auth -o jsonpath='{.data.password}' | base64 -d) dbname=postgres sslmode=require"

diagrid agent create invoice-approver --project verifiable-execution-demo --wait
```

### 3. Agent

```bash
kve create secret generic catalyst-invoice-approver \
  --from-literal=apiToken="$(diagrid appid get invoice-approver -p verifiable-execution-demo -o json | jq -r .status.apiToken)" \
  --from-literal=grpcEndpoint="$(diagrid project get verifiable-execution-demo -o json | jq -r .status.endpoints.grpc.url)" \
  --from-literal=httpEndpoint="$(diagrid project get verifiable-execution-demo -o json | jq -r .status.endpoints.http.url)"

kve create secret generic openai --from-literal=apiKey="$OPENAI_API_KEY"
```

```bash
kubectl --context demo-production-catalyst apply -f deploy/deployment.yaml
kve logs deploy/invoice-approver | grep -E "Outcome|Worker still connected"
```

If the log shows `failed to connect to task hub service ... Connection refused`, the sidecar is
still restarting after the component change. Wait a minute, then run `kve rollout restart
deploy/invoice-approver`.

## Run the demo

Every pod start is a new run. `kve rollout restart deploy/invoice-approver` starts another one.
Find the instance ID to tamper with:

```bash
diagrid workflow list -p verifiable-execution-demo
```

Tamper with it through a port-forward:

```bash
kve port-forward svc/postgres 55432:5432 &
export PGHOST=127.0.0.1 PGPORT=55432 PGUSER=postgres PGDATABASE=postgres INSTANCE_ID=<instance-id>
export PGPASSWORD="$(kve get secret postgres-auth -o jsonpath='{.data.password}' | base64 -d)"
uv run --with 'psycopg[binary]' python tamper_history.py           # dry run
uv run --with 'psycopg[binary]' python tamper_history.py --apply
```

Then follow step 5 of the tutorial. The console shows the instance as tampered, and
`diagrid workflow rerun ... --instance-id <instance-id>` is refused. The worker is always
running, so console reruns work at any time.

## Who can reach the database

The `postgres` Service only accepts connections from `54.155.61.100`, the `diagrid-aws-eu-west`
region's egress address. Catalyst publishes it:

```bash
diagrid region get diagrid-aws-eu-west -o json | jq .status.egressAddresses
```

If that list changes, `wf-store` fails until the new address is added to
`loadBalancerSourceRanges` in `postgres.yaml`. Postgres logs every client address, which shows
where Catalyst connects from:

```bash
kve logs postgres-0 | grep -oE 'connection received: host=[0-9.]+' | sort | uniq -c
```

Remote logins must use SSL and the generated password. Logins from inside the pod, which
includes `kubectl port-forward`, need no password, so cluster access is the gate for the tamper
script.

## Tear down

```bash
kubectl --context demo-production-catalyst delete namespace verifiable-execution-demo
diagrid project delete verifiable-execution-demo
```

Deleting the namespace deletes the disk and every run's history with it.

## Don't

- Set `INSTANCE_ID` on the Deployment. After that instance is tampered with, every restart is
  refused and the pod crash-loops.
- Widen `.dockerignore`. The image is public; it allowlists the three files the build needs.
- Recreate the `postgres` Service without the DNS label annotation. The component's connection
  string uses that hostname.
