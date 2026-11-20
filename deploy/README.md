# Deploying replaygate to one EC2 server

One Ubuntu server runs three containers: Postgres, the API, and Caddy (which
serves the dashboard and gets the HTTPS certificate by itself). Only Caddy is
reachable from the internet. Postgres and the API are not published.

```
internet --443--> Caddy --/v1 /auth /healthz--> API --> Postgres
                    |
                    +--everything else--> dashboard files
```

## 0. Try the stack on your own machine first (5 minutes)

This checks the Docker build before you pay for a server. Docker Desktop must be
running.

```bash
cd ~/replaygate
POSTGRES_PASSWORD=preflight RG_HOST=http://localhost RG_PUBLIC_URL=http://localhost:8088 \
  RG_HTTP_PORT=8088 RG_HTTPS_PORT=8443 \
  docker compose -f docker-compose.prod.yml up -d --build
```
```bash
curl -s http://localhost:8088/healthz          # {"status":"ok"}
curl -sI http://localhost:8088/ | head -1      # HTTP/1.1 200 OK
```
Open http://localhost:8088 to see the sign-in page (sign-in itself is off here).
Then remove it, including its throwaway database:
```bash
POSTGRES_PASSWORD=preflight RG_HOST=x RG_PUBLIC_URL=x docker compose -f docker-compose.prod.yml down -v
```

## 1. Launch the server (AWS console, region Asia Pacific (Mumbai))

EC2 > Launch instance:

| Setting | Value |
|---|---|
| Name | replaygate |
| Image | Ubuntu Server 24.04 LTS, 64-bit (x86) |
| Instance type | t3.small (2 GB RAM: the Go and Node builds need it) |
| Key pair | Create new, type ED25519, format .pem. Keep the downloaded file private. |
| Network | Create a security group: SSH from **My IP**; HTTPS and HTTP from **Anywhere** |
| Storage | 20 GiB gp3 |

Do not open port 5432 or 8080. Caddy needs 80 as well as 443: Let's Encrypt
uses it to prove the address is yours.

Then EC2 > Elastic IPs > Allocate, and associate it with the instance, so the
address survives a stop and start. Note the IP; the rest of this guide calls it
`<IP>`. If your own IP changes (new Wi-Fi), edit the SSH rule of the security group.

## 2. Connect and install Docker (from your Ubuntu/WSL terminal)

```bash
mkdir -p ~/.ssh && cp /mnt/c/Users/poorv/Downloads/<key-file>.pem ~/.ssh/ && chmod 400 ~/.ssh/<key-file>.pem
ssh -i ~/.ssh/<key-file>.pem ubuntu@<IP>
```
On the server:
```bash
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 git
sudo usermod -aG docker ubuntu
exit
```
Connect again (the group change needs a new login), then:
```bash
git clone https://github.com/Sangsty/replaygate.git && cd replaygate
```

## 3. Configure

```bash
bash deploy/init-env.sh <IP>          # writes .env.prod with generated secrets
```
It prints the dashboard address (your IP with dashes plus `.sslip.io`) and the
OAuth callback URL.

Create the GitHub OAuth app **before** starting the stack, because the API
refuses to start with a JWT secret but no OAuth app:

1. https://github.com/settings/developers > OAuth Apps > New OAuth App.
   Homepage URL: `https://<host>`. Redirect URI: `https://<host>/auth/github/callback`.
   Leave wildcard matching and device flow off.
2. Generate a client secret (GitHub shows it once) and put the client id and
   secret into `.env.prod`:
   ```bash
   nano .env.prod        # fill RG_GITHUB_CLIENT_ID and RG_GITHUB_CLIENT_SECRET
   ```

## 4. Start and create your project

```bash
deploy/compose.sh up -d --build       # first build takes 5-10 minutes
deploy/compose.sh ps                  # api and postgres "healthy"
curl -s https://<IP-with-dashes>.sslip.io/healthz
```
Caddy asks Let's Encrypt for a certificate on the first visit; if `curl` fails
once, wait a minute and retry (`deploy/compose.sh logs web` shows progress).

Create your project and give your GitHub account access:
```bash
deploy/compose.sh run --rm api bootstrap -name replaygate-demo -owner <your-github-login>
```
It prints `RG_PROJECT_ID` and `RG_API_KEY` once. Save the key (a password
manager, or a GitHub secret); only its hash is stored.

## 5. Put demo data in

From your own machine, in `~/replaygate` (nothing is copied to the server):
```bash
RG_API_URL=https://<host> RG_PROJECT_ID=<id> RG_API_KEY=<key> bash scripts/seed-demo.sh
```
Then sign in at `https://<host>`: one failed run with three breaking changes
and one passed run.

## Operating it

```bash
deploy/compose.sh logs -f api                   # follow the API log
deploy/compose.sh ps                            # health
git pull && deploy/compose.sh up -d --build     # update to the latest commit
deploy/compose.sh exec -T postgres pg_dump -U replaygate replaygate | gzip > backup-$(date +%F).sql.gz
```

Secrets live only in `.env.prod` (mode 600, git-ignored) on the server. Do not
paste it anywhere. To rotate `RG_JWT_SECRET`, edit it and run `deploy/compose.sh up -d`;
everyone is signed out.

## Cost and shutting down

The instance and its public IPv4 address are billed while they exist; check the
price shown in the launch screen and set a billing alert (Billing > Budgets).
A stopped instance still pays for its Elastic IP and disk. To delete everything:
terminate the instance, then release the Elastic IP.

## Known limits of this setup

- One server, one Postgres container, no replicas. A reboot interrupts service for a minute.
- Backups are the `pg_dump` command above, run by hand.
- The API has no rate limiting; Caddy is the only thing in front of it.
- An `.sslip.io` name is fine for a demo. Use your own domain for anything customers see
  (set `RG_HOST` and `RG_PUBLIC_URL` to it and point its A record at the Elastic IP).
