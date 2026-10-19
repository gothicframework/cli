package tfgen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gothicframework/core/config"
)

// GcpResources returns the base resources.tf.json for GCP: the Cloud Run
// service, the GCS asset bucket, and — when the stage declares a CustomDomain —
// the Cloud Run domain mapping that serves it with a Google-managed TLS
// certificate (provisioned automatically once the domain is verified).
//
// Everything the config object carries for GCP is materialized here:
//   - params.EnvVars  → the container's environment (on top of GOTHIC_PROVIDER)
//   - params.CustomDomain → enable_domain_mapping + local.custom_domain
//   - params.Project/Region → provider wiring (main.tf.json)
//
// Key differences from AWS:
//   - Cloud Run service instead of Lambda Function
//   - GAR instead of ECR
//   - GCS bucket instead of S3
//   - No CloudFront CDN (static assets served directly or via Cloud CDN)
//   - No VPC attachment needed (Cloud Run handles networking)
//
// Memory, timeout, and concurrency use Cloud Run's service defaults; the
// container maps HTTP port 8080 (the Cloud Run $PORT contract).
func GcpResources(params TfGenParams) map[string]interface{} {
	// The container's env list: every config ENV entry resolved through
	// local.env_vars (raw values land here as plain strings; GCP rejects
	// SSM/Secrets Manager sources at resolution time), plus GOTHIC_PROVIDER
	// telling the runtime it's running on GCP.
	env := []interface{}{
		map[string]interface{}{
			"name":  "GOTHIC_PROVIDER",
			"value": "GCP",
		},
	}
	keys := make([]string, 0, len(params.EnvVars))
	for k := range params.EnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, map[string]interface{}{
			"name":  k,
			"value": fmt.Sprintf(`local.env_vars["%s"]`, k),
		})
	}

	resources := map[string]interface{}{
		// Cloud Run service — the main compute.
		"google_cloud_run_service": map[string]interface{}{
			"main": map[string]interface{}{
				"name":     "${var.service_name}",
				"location": "${var.region}",
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"container_concurrency": 100,
						"containers": []interface{}{
							map[string]interface{}{
								"image": "${var.gcp_image_uri}",
								"ports": []interface{}{
									map[string]interface{}{
										"container_port": 8080,
									},
								},
								"env": env,
							},
						},
					},
				},
			},
		},
		// GCS bucket for static assets (/public/*), served directly or via
		// Cloud CDN.
		"google_storage_bucket": map[string]interface{}{
			"main": map[string]interface{}{
				"name":          "${var.bucket_name}",
				"location":      "${var.region}",
				"storage_class": "STANDARD",
			},
		},
	}

	// Custom domain: the Cloud Run domain mapping provisions the Google-managed
	// TLS certificate (the user points DNS at the emitted record). Gated on the
	// plan-time enable_domain_mapping boolean; the domain VALUE is resolved into
	// local.custom_domain by env resolution.
	if params.CustomDomain != nil {
		resources["google_cloud_run_domain_mapping"] = map[string]interface{}{
			"main": map[string]interface{}{
				"count":    "${var.enable_domain_mapping ? 1 : 0}",
				"location": "${var.region}",
				"name":     "${local.custom_domain}",
				"spec": map[string]interface{}{
					"route_record_name": "${google_cloud_run_service.main.status[0].url}",
				},
			},
		}
	}

	return resources
}

// WriteGcpResources writes resources.tf.json into workDir from GcpResources.
// The GCP resource set is generated rather than embedded because the container
// env list depends on the stage's config ENV entries.
func WriteGcpResources(workDir string, params TfGenParams) error {
	return writeJSON(filepath.Join(workDir, "resources.tf.json"),
		map[string]any{"resource": GcpResources(params)})
}

// WriteGcpOutputs writes gothic_outputs.tf.json for GCP: a `locals` block
// re-exporting the Gothic stack's key resource attributes under STABLE
// `gothic_*` names. This is the public contract user infra/ files build
// against (attaching a Cloud Armor policy, granting a service account access
// to the bucket) — it decouples them from Gothic's internal resource
// addresses, which may change.
func WriteGcpOutputs(workDir string) error {
	locals := map[string]any{
		"gothic_service_name": "${google_cloud_run_service.main.name}",
		"gothic_service_url":  "${google_cloud_run_service.main.status[0].url}",
		"gothic_bucket_name":  "${google_storage_bucket.main.name}",
	}
	return writeJSON(filepath.Join(workDir, "gothic_outputs.tf.json"), map[string]any{"locals": locals})
}

// gcpResolve turns an EnvValue into the tf expression the GCP config uses.
// Only raw literals are supported today: GCP data-source resolution (Secret
// Manager backed ENV and domains) is not wired, and silently producing an AWS
// data-source reference in a google-only stack would break at plan time — so an
// SSM/Secrets Manager source fails loudly instead.
func gcpResolve(kind, key string, v config.EnvValue) (string, error) {
	switch v.Source {
	case config.SSMParamEnv, config.SecretsManagerEnv:
		return "", fmt.Errorf("%s %q uses a source GCP cannot resolve (only gothic.Env raw values are supported on GCP yet)", kind, key)
	default: // RawEnv
		return v.Value, nil
	}
}

// GcpEnvLocals resolves the stage's ENV map into the locals.env_vars values,
// failing loudly when a value uses a source GCP cannot resolve.
func GcpEnvLocals(env map[string]config.EnvValue) (map[string]string, error) {
	out := make(map[string]string, len(env))
	for k, v := range env {
		expr, err := gcpResolve("ENV", k, v)
		if err != nil {
			return nil, err
		}
		out[k] = expr
	}
	return out, nil
}

// GcpDomainLocal resolves the custom-domain EnvValue into local.custom_domain,
// with the same raw-only restriction as the env entries. An empty string keeps
// resources that reference the local resolvable when no domain is declared.
func GcpDomainLocal(v *config.EnvValue) (string, error) {
	if v == nil {
		return "", nil
	}
	return gcpResolve("CustomDomain", "custom_domain", *v)
}

// QuoteBracketKey wraps a locals map key into the bracket interpolation
// local.env_vars["K"] — safe for keys that are not valid TF identifiers.
func QuoteBracketKey(key string) string {
	return `local.env_vars["` + strings.ReplaceAll(key, `"`, `\"`) + `"]`
}
