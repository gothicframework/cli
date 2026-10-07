---
name: gothic-deploy-aws
description: "Gothic v3 deploy: gothic deploy --action deploy|delete --stage <stage>, OpenTofu (Lambda container + Function URL + CloudFront + S3), AWS provider surface, ENV/secret sources, custom domains, CDN cache rules, infra/ drop-in."
applies_to: "core@>=1.0.0,<2.0.0 middlewares@>=1.3.0"
---

# Gothic deploy (AWS)

## How it works

```bash
gothic deploy --action deploy --stage dev   # provision / update
gothic deploy --action delete --stage dev   # tear the whole stack down
```

A deploy: builds front-end assets (Templ → Go, Tailwind, per-page WASM) → builds a container image and pushes it to **ECR** → applies an **OpenTofu** stack (S3 state backend + DynamoDB lock table, bootstrapped automatically on first deploy) → uploads `public/` to **S3** → invalidates the CloudFront cache (`/*`).

### Architecture

```
viewer ──► CloudFront
             /public/*  ──► S3 bucket (Origin Access Control)
             everything ──► Lambda Function URL (container, IAM-auth + SigV4 from CloudFront)
```

- App routes → a container **Lambda** behind a **Function URL**, fronted by CloudFront with **OAC + SigV4** (the Function URL is IAM-auth; only CloudFront can invoke it).
- `/public/*` → **S3** via OAC. `/_gothic/*` (WASM runtime assets) and `/optimizedImage/*` → the Lambda.
- Optional custom domain → CloudFront alias + ACM certificate in us-east-1.

### The `Deploy` block in `gothic.config.go`

```go
Deploy: &gothic.DeployConfig{
    Provider: gothic.AWS, // only provider in this version; field is future-proofing
    Providers: gothic.Providers{
        AWS: gothic.AWSProvider{
            ServerMemory:  512,       // Lambda memory (MB)
            ServerTimeout: 30,        // Lambda timeout (s)
            Region:        "us-east-1",
            Profile:       "default", // local AWS profile for the deploy
            Stages: map[string]gothic.Stage{
                "dev": {
                    CustomDomain: gothic.Env("app.example.com"),        // optional
                    HostedZoneId: gothic.Env("Z0123456789ABCDEFGHIJ"),  // optional
                    ENV: map[string]gothic.EnvValue{
                        "TABLE_NAME": gothic.Env("my-app-dev-items"),
                        "API_KEY":    gothic.SecretsManager("/my-app/dev/api-key").Get("secret-key"),
                        "DB_URL":     gothic.SSMParam("/my-app/dev/db-url"),
                    },
                },
            },
        },
    },
},
```

Source-aware values (`ENV` + domain/cert fields): `gothic.Env("literal")`, `gothic.SSMParam("/path")`, `gothic.SecretsManager("/path")` (+ `.Get("field")` for a JSON field) — resolved at deploy time so secrets are never committed.

### Custom domain + HTTPS

Set `CustomDomain` **and** `HostedZoneId` → a DNS-validated ACM certificate is created for you (us-east-1, regardless of the stack region). Set `CertificateArn` to reuse an existing us-east-1 cert instead; `WafArn` optionally attaches a WAF web ACL. Route 53 hosted-zone IDs are all-uppercase — a lowercase character in `HostedZoneId` is the usual cause of a failed custom-domain deploy.

### CDN cache config — `Providers.AWS.CDN`

By default CloudFront folds **every query param** into the cache key and forwards **no cookies/headers** to the Lambda origin. Tune it per field with an `AllowRule` builder (never construct one directly):

| Builder | Meaning |
|---|---|
| `gothic.AllowAll()` | every value forwarded + in the cache key |
| `gothic.AllowNone()` | nothing forwarded or cached on |
| `gothic.Allow("a", "b")` | whitelist |
| `gothic.AllowAllExcept("x")` | everything except the named (NOT valid for Headers) |

- Zero-value defaults per field: `QueryParams` → `AllowAll`, `Cookies` → `AllowNone`, `Headers` → `AllowNone`.
- **Headers accept only `AllowNone()` or `Allow(...)`** — `AllowAll()`/`AllowAllExcept()` are rejected at deploy time (CloudFront cache-policy limit).
- **Never `Allow("Host")` or `Allow("Authorization")`** on the server behavior — the OAC SigV4 signature is computed against the Function URL's own host; forwarding either breaks it (HTTP 403).

ISR routes emit `Cache-Control: max-age=N, stale-while-revalidate=N, stale-if-error=N` — honored by CloudFront's cache policy — so ISR/STATIC pages edge-cache; DYNAMIC pages send `no-store` and always hit the Lambda.

### `infra/` — add your own AWS resources

Drop top-level `.tf` (HCL) or `.tf.json` files into an `infra/` folder at the project root: they merge flat into the SAME OpenTofu module and state as the Gothic stack — one plan/apply, and `--action delete` tears them down too.

- Only top-level files are merged; subfolders/READMEs ignored. A file whose base name collides with a Gothic-generated file (`resources.tf.json`, `main.tf.json`, `variables.tf.json`, `outputs.tf.json`, `gothic_outputs.tf.json`) is **rejected** — rename it.
- Reference the Gothic stack only through the stable **`local.gothic_*` contract**: `gothic_lambda_role_{name,arn}`, `gothic_lambda_function_{name,arn}`, `gothic_s3_bucket_{name,arn}`, `gothic_cloudfront_distribution_id`, `gothic_cloudfront_domain_name` — never internal resource addresses.
- Name resources with `${var.project_name}` / `${var.stage}` so stages don't collide. The Lambda's execution role already holds its credentials — the AWS SDK needs no keys.

### Naming and stage rules

All three of ECR repo, asset bucket, and Lambda are named `project-stage-suffix` (deterministic, per stage). The state bucket and lock table are `project-state-suffix` / `project-lock-suffix` — **shared across every stage of the project**; deleting them orphans other stages. `deploy` to a stage not declared in `Deploy.Stages` is refused; `delete` proceeds with a warning (so a renamed/orphaned stage can still be torn down). Stages are alphanumeric only (`^[a-zA-Z0-9]+$`) — no dashes or underscores.

## What the agent CAN do

- Run `gothic deploy` / `gothic delete` per declared stage; wire stages with per-stage ENV, domains, and WAF.
- Configure Lambda memory/timeout, region, and the local AWS profile.
- Tune the CloudFront cache key/forwarding per field with the four `AllowRule` builders.
- Attach a custom domain (managed cert via `CustomDomain`+`HostedZoneId`, or existing `CertificateArn`).
- Add AWS resources via `infra/` and grant the app's Lambda role policies on them.
- Run deploy hooks (`BeforeDeploy`/`AfterDeploy`) to validate or consume stack outputs.

## What the agent CANNOT do

- Deploy to an undeclared stage, or use a stage name with dashes/underscores.
- Put `AllowAll()`/`AllowAllExcept()` on `Headers`, or forward `Host`/`Authorization` — rejected or signature-breaking.
- Edit generated `.tf.json` under `.gothicCli/` — regenerated each deploy; put resources in `infra/`.
- Reference Gothic's internal OpenTofu resource addresses from `infra/` — use the `local.gothic_*` contract.
- Use a non-`AWS` provider — only `gothic.AWS` exists in this version.
- Delete the shared state bucket/lock table expecting only one stage to be affected — they are per-project.
- Expect deploy hooks to run on `--action delete` — both hooks are deploy-only.
