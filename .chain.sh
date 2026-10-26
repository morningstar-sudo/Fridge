IMGS="fridge:dev fridge-job:dev postgres:17 postgres:18 traefik:v3.5 cgr.dev/chainguard/minio:latest cgr.dev/chainguard/minio-client:latest"
rm -f /tmp/mysql4.out
docker build -q -f deploy/Dockerfile -t fridge:dev . >/dev/null
bash scripts/swarm-test/down.sh >/dev/null; bash scripts/swarm-test/up.sh $IMGS >/dev/null 2>&1
bash scripts/swarm-test/gotest.sh app 'TestSwarmEngines/mysql' > /tmp/mysql4.out 2>&1; echo EXIT=$? >> /tmp/mysql4.out
