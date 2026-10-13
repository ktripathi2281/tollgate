# Deploying the demo: Render and Supabase

The hosted demo runs on Render's free tier, with Postgres on Supabase's free tier. Both cost nothing, and both come with limits worth knowing first:

- **Cold starts.** A free Render service spins down after 15 minutes without traffic. The next request wakes it, which takes about a minute.
- **One small instance.** 0.1 CPU and 512 MB of memory, one instance. That suits Tollgate, which is single-instance anyway (its rate limits live in memory), but it is a demo, not production capacity.
- **Database pauses.** A free Supabase project pauses after 7 days without activity. Resume it from the Supabase dashboard; until then the gateway answers `/readyz` with 503 and API requests with 503 `auth_unavailable`.
- **No shell, no pre-deploy command.** Migrations and key management run from your machine, against the same database.

The files involved are `render.yaml` (the Render Blueprint), `Dockerfile`, and `deploy/demo.yaml` (the gateway's config for the demo).

## 1. Create the database

1. Create a Supabase project. Pick the region nearest the users, and note it: the Render service should be in the same one. `render.yaml` says `singapore`.
2. In the project's **Connect** dialog, copy the **Session pooler** connection string. It looks like:

   ```
   postgres://postgres.<project-ref>:<password>@aws-<n>-<region>.pooler.supabase.com:5432/postgres
   ```

   Use the session pooler (port 5432), not the transaction pooler (port 6543). The gateway is a long-running server, and it relies on two things transaction mode doesn't support: prepared statements, which pgx uses by default, and session-level advisory locks, which `tollgate migrate` uses so concurrent runs take turns. The direct connection (`db.<project-ref>.supabase.co`) would work too, but it is IPv6-only on the free plan.

3. Add `?sslmode=require&pool_max_conns=5` to the end. The first insists on TLS; the second keeps the gateway's pool small, well inside the free pooler's connection limit.

Keep this URL secret. It goes into Render's settings and your shell, never into the repository.

## 2. Apply the migrations and create the demo key

From WSL, in the repository:

```bash
make build
export DATABASE_URL='postgres://postgres.<project-ref>:<password>@aws-<n>-<region>.pooler.supabase.com:5432/postgres?sslmode=require&pool_max_conns=5'
./bin/tollgate migrate
./bin/tollgate keys create -name public-demo -models mock-fast,mock-flaky -rpm 10 -tpm 20000 -budget 0.10 -period daily -config deploy/demo.yaml
```

The demo key can use only the mock aliases, at 10 requests and 20,000 tokens a minute, with a $0.10 daily budget at the mock's fake prices. It is safe to publish. A key for the real `fast` alias should never be published; create it separately if you add a provider key.

Run `tollgate migrate` again whenever a release adds a migration, before deploying it: the gateway refuses to start on an out-of-date schema.

## 3. Create the Render service

1. Push the repository to GitHub, and in Render choose **New → Blueprint**, then the repository. Render reads `render.yaml`.
2. Render asks for the variables marked `sync: false`:
   - `DATABASE_URL`: the URL from step 1.
   - `ANTHROPIC_API_KEY`: leave empty for a mock-only demo. If set, the `fast` alias works for keys allowed to use it. Render keeps it in its secret store; it never appears in the repository or the logs.
3. Apply. Render builds the Docker image and starts `tollgate serve --config /etc/tollgate/demo.yaml`. `PORT=8080` tells Render where the gateway listens, and the health check is `/healthz`.

Each push to the default branch deploys again (`autoDeployTrigger: commit`). On a deploy Render sends the old instance SIGTERM and kills it 30 seconds later; `deploy/demo.yaml` gives in-flight requests 20 seconds of that, so holds are settled and request logs written before it exits.

## 4. Check it

```bash
URL=https://tollgate-<something>.onrender.com   # from the Render dashboard
curl -s $URL/readyz
curl -s $URL/v1/chat/completions \
  -H "Authorization: Bearer $DEMO_KEY" -H 'Content-Type: application/json' \
  -d '{"model": "mock-fast", "messages": [{"role": "user", "content": "Hello"}]}'
./bin/tollgate reconcile   # with DATABASE_URL still set: the books balance
```

The first request after a quiet spell takes about a minute while the service wakes.

## What stays private

- `/metrics` and `/debug/pprof/` listen on `127.0.0.1:6060` inside the container. Render forwards public traffic to one port only, and free services take no private-network traffic, so nothing outside reaches them. To look at them, run the image locally (`docker compose up`) or move to a paid instance with a private service in front.
- Logs, in Render's dashboard, hold request metadata only: never keys, prompts or completions.

## Moving off the free tiers

Nothing in the gateway changes. A paid Render instance (or any container host that keeps long-lived HTTP responses open) removes the cold starts; Supabase Pro removes the pause. Keep one instance: the rate limiter is in memory (see the README's limitations).
