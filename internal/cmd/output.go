package cmd

import (
	"fmt"
	"strings"

	"github.com/gothicframework/cli/v3/internal/termcolor"
)

// Local shorthands for the shared Gothic palette (pkg/helpers/termcolor) so the
// formatting below stays terse. The color values and the enable check live in
// exactly one place — change a color there and it changes across every CLI log.
const (
	clrReset  = termcolor.Reset
	clrBold   = termcolor.Bold
	clrUnder  = termcolor.Under
	clrDim    = termcolor.Gray   // labels / secondary text
	clrGreen  = termcolor.Green  // success
	clrCyan   = termcolor.Cyan   // values / URLs
	clrPurple = termcolor.Purple // Gothic accent / rules
	clrYellow = termcolor.Yellow // warnings
	clrRed    = termcolor.Red    // errors
)

// paint wraps s in the given ANSI codes, honoring the shared enable check.
func paint(codes, s string) string { return termcolor.Paint(codes, s) }

// deployBanner prints the header shown once at the start of a deploy/delete run.
func deployBanner(action, stage string) {
	verb := "Deploying"
	if action == "delete" {
		verb = "Removing"
	}
	fmt.Println()
	fmt.Println(
		"  " + paint(clrBold+clrPurple, "◆ Gothic") +
			paint(clrDim, "  ·  "+verb+" stage ") +
			paint(clrBold+clrCyan, stage),
	)
}

// deployPhase prints a colored section header before a major deploy step so the
// otherwise-flat stream of Docker/OpenTofu output is grouped into readable phases.
func deployPhase(title string) {
	fmt.Println()
	fmt.Println(paint(clrBold+clrPurple, "▸ ") + paint(clrBold, title))
}

// lastSeg returns the substring after the last occurrence of sep (used to turn an
// ARN into its short resource name), or s unchanged when sep is absent.
func lastSeg(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[i+len(sep):]
	}
	return s
}

// summaryRow prints one aligned "emoji  label  value" line in the result box.
func summaryRow(emoji, label, value, valueCodes string) {
	if value == "" {
		return
	}
	fmt.Printf("  %s  %s  %s\n", emoji, paint(clrDim, fmt.Sprintf("%-12s", label)), paint(valueCodes, value))
}

// printDeploySummary prints the structured, colored result box after a successful
// deploy, leading with the live URL the user opens.
func printDeploySummary(stage string, outputs map[string]string) {
	rule := strings.Repeat("━", 56)
	cfURL := ""
	if d := outputs["cloudfront_domain_name"]; d != "" {
		cfURL = "https://" + d
	}
	customURL := ""
	if d := outputs["custom_domain"]; d != "" {
		customURL = "https://" + d
	}
	// When a custom domain is configured it is the address users actually visit, so
	// lead with it; the CloudFront URL is still shown (the custom domain needs DNS
	// to resolve, and a managed cert to finish validating, before it works).
	primary := cfURL
	if customURL != "" {
		primary = customURL
	}

	fmt.Println()
	fmt.Println(paint(clrPurple, rule))
	fmt.Println("  " + paint(clrBold+clrGreen, "✓ Deploy complete") + paint(clrDim, "   ·   stage ") + paint(clrBold+clrCyan, stage))
	fmt.Println()
	if primary != "" {
		fmt.Println("  🌐  " + paint(clrDim, "Open your site") + paint(clrDim, "  →  ") + paint(clrBold+clrCyan+clrUnder, primary))
		if customURL != "" && cfURL != "" {
			fmt.Println("      " + paint(clrDim, "via CloudFront   ") + paint(clrDim, cfURL))
		}
		fmt.Println()
	}
	summaryRow("⚡", "Lambda", lastSeg(outputs["lambda_function_arn"], ":"), clrCyan)
	summaryRow("🪣", "Assets (S3)", lastSeg(outputs["s3_bucket_arn"], ":"), clrCyan)
	summaryRow("📡", "CloudFront", outputs["cloudfront_distribution_id"], clrCyan)
	fmt.Println(paint(clrPurple, rule))
	fmt.Println()
}

// printDeleteSummary prints the result box after a successful teardown.
func printDeleteSummary(stage string) {
	rule := strings.Repeat("━", 56)
	fmt.Println()
	fmt.Println(paint(clrPurple, rule))
	fmt.Println("  " + paint(clrBold+clrGreen, "✓ Teardown complete") + paint(clrDim, "   ·   stage ") + paint(clrBold+clrCyan, stage))
	fmt.Println("  " + paint(clrDim, "All AWS resources for this stage were destroyed."))
	fmt.Println(paint(clrPurple, rule))
	fmt.Println()
}
