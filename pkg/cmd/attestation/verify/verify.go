package verify

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/cli/cli/v2/internal/text"
	"github.com/cli/cli/v2/pkg/cmd/attestation/api"
	"github.com/cli/cli/v2/pkg/cmd/attestation/artifact"
	"github.com/cli/cli/v2/pkg/cmd/attestation/artifact/oci"
	"github.com/cli/cli/v2/pkg/cmd/attestation/auth"
	"github.com/cli/cli/v2/pkg/cmd/attestation/io"
	"github.com/cli/cli/v2/pkg/cmd/attestation/verification"
	"github.com/cli/cli/v2/pkg/cmdutil"

	"github.com/MakeNowJust/heredoc"
	"github.com/spf13/cobra"
)

func NewVerifyCmd(f *cmdutil.Factory, runF func(*Options) error) *cobra.Command {
	opts := &Options{}
	verifyCmd := &cobra.Command{
		Use:   "verify [<file-path> | oci://<image-uri>] [--owner | --repo]",
		Args:  cmdutil.ExactArgs(1, "must specify file path or container image URI, as well as one of --owner or --repo"),
		Short: "Verify an artifact's integrity using attestations",
		Long: heredoc.Docf(`
			### NOTE: This feature is currently in beta, and subject to change.

			Verify the integrity and provenance of an artifact using its associated
			cryptographically signed attestations.

			The command requires either:
			* a file path to an artifact, or
			* a container image URI (e.g. %[1]soci://<image-uri>%[1]s)
			  * (note that if you provide an OCI URL, you must already be authenticated with
			its container registry)

			In addition, the command requires either:
			* the %[1]s--repo%[1]s flag (e.g. --repo github/example).
			* the %[1]s--owner%[1]s flag (e.g. --owner github), or

			The %[1]s--repo%[1]s flag value must match the name of the GitHub repository
			that the artifact is linked with.

			The %[1]s--owner%[1]s flag value must match the name of the GitHub organization
			that the artifact's linked repository belongs to.

			By default, the verify command will attempt to fetch attestations associated
			with the provided artifact from the GitHub API. If you would prefer to verify
			the artifact using attestations stored on disk (c.f. the %[1]sdownload%[1]s command),
			provide a path to the %[1]s--bundle%[1]s flag.

			To see the full results that are generated upon successful verification, i.e.
			for use with a policy engine, provide the %[1]s--json-result%[1]s flag.

			The attestation's certificate's Subject Alternative Name (SAN) identifies the entity
			responsible for creating the attestation, which most of the time will be a GitHub
			Actions workflow file located inside your repository. By default, this command uses
			either the %[1]s--repo%[1]s or the %[1]s--owner%[1]s flag value to validate the SAN.

			However, if you generate attestations with a reusable workflow then the SAN will
			identify the reusable workflow – which may or may not be located inside your %[1]s--repo%[1]s
			or %[1]s--owner%[1]s. In these situations, you can use the %[1]s--cert-identity%[1]s or
			%[1]s--cert-identity-regex%[1]s flags to specify the reusable workflow's URI.

			For more policy verification options, see the other available flags.
			`, "`"),
		Example: heredoc.Doc(`
			# Verify a local artifact linked with a repository
			$ gh attestation verify example.bin --repo github/example

			# Verify a local artifact linked with an organization
			$ gh attestation verify example.bin --owner github

			# Verify an OCI image using locally stored attestations
			$ gh attestation verify oci://<image-uri> --owner github --bundle sha256:foo.jsonl
		`),
		// PreRunE is used to validate flags before the command is run
		// If an error is returned, its message will be printed to the terminal
		// along with information about how use the command
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Create a logger for use throughout the verify command
			opts.Logger = io.NewHandler(f.IOStreams)

			// set the artifact path
			opts.ArtifactPath = args[0]

			// Check that the given flag combination is valid
			if err := opts.AreFlagsValid(); err != nil {
				return err
			}

			// Clean file path options
			opts.Clean()

			// set policy flags based on what has been provided
			opts.SetPolicyFlags()

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			hc, err := f.HttpClient()
			if err != nil {
				return err
			}
			opts.APIClient = api.NewLiveClient(hc, opts.Logger)

			opts.OCIClient = oci.NewLiveClient()

			if err := auth.IsHostSupported(); err != nil {
				return err
			}

			if runF != nil {
				return runF(opts)
			}

			config := verification.SigstoreConfig{
				CustomTrustedRoot: opts.CustomTrustedRoot,
				Logger:            opts.Logger,
				NoPublicGood:      opts.NoPublicGood,
			}

			opts.SigstoreVerifier = verification.NewLiveSigstoreVerifier(config)

			if err := runVerify(opts); err != nil {
				return fmt.Errorf("\nError: %v", err)
			}
			return nil
		},
	}

	// general flags
	verifyCmd.Flags().StringVarP(&opts.BundlePath, "bundle", "b", "", "Path to bundle on disk, either a single bundle in a JSON file or a JSON lines file with multiple bundles")
	cmdutil.DisableAuthCheckFlag(verifyCmd.Flags().Lookup("bundle"))
	cmdutil.StringEnumFlag(verifyCmd, &opts.DigestAlgorithm, "digest-alg", "d", "sha256", []string{"sha256", "sha512"}, "The algorithm used to compute a digest of the artifact")
	verifyCmd.Flags().StringVarP(&opts.Owner, "owner", "o", "", "GitHub organization to scope attestation lookup by")
	verifyCmd.Flags().StringVarP(&opts.Repo, "repo", "R", "", "Repository name in the format <owner>/<repo>")
	verifyCmd.MarkFlagsMutuallyExclusive("owner", "repo")
	verifyCmd.MarkFlagsOneRequired("owner", "repo")
	verifyCmd.Flags().StringVarP(&opts.PredicateType, "predicate-type", "", "", "Filter attestations by provided predicate type")
	verifyCmd.Flags().BoolVarP(&opts.NoPublicGood, "no-public-good", "", false, "Only verify attestations signed with GitHub's Sigstore instance")
	verifyCmd.Flags().StringVarP(&opts.CustomTrustedRoot, "custom-trusted-root", "", "", "Path to a custom trustedroot.json file to use for verification")
	verifyCmd.Flags().IntVarP(&opts.Limit, "limit", "L", api.DefaultLimit, "Maximum number of attestations to fetch")
	cmdutil.AddFormatFlags(verifyCmd, &opts.exporter)
	// policy enforcement flags
	verifyCmd.Flags().BoolVarP(&opts.DenySelfHostedRunner, "deny-self-hosted-runners", "", false, "Fail verification for attestations generated on self-hosted runners")
	verifyCmd.Flags().StringVarP(&opts.SAN, "cert-identity", "", "", "Enforce that the certificate's subject alternative name matches the provided value exactly")
	verifyCmd.Flags().StringVarP(&opts.SANRegex, "cert-identity-regex", "i", "", "Enforce that the certificate's subject alternative name matches the provided regex")
	verifyCmd.MarkFlagsMutuallyExclusive("cert-identity", "cert-identity-regex")
	verifyCmd.Flags().StringVarP(&opts.OIDCIssuer, "cert-oidc-issuer", "", GitHubOIDCIssuer, "Issuer of the OIDC token")

	return verifyCmd
}

func runVerify(opts *Options) error {
	artifact, err := artifact.NewDigestedArtifact(opts.OCIClient, opts.ArtifactPath, opts.DigestAlgorithm)
	if err != nil {
		opts.Logger.Printf(opts.Logger.ColorScheme.Red("✗ Loading digest for %s failed\n"), opts.ArtifactPath)
		return err
	}

	opts.Logger.Printf("Loaded digest %s for %s\n", artifact.DigestWithAlg(), artifact.URL)

	c := verification.FetchAttestationsConfig{
		APIClient:  opts.APIClient,
		BundlePath: opts.BundlePath,
		Digest:     artifact.DigestWithAlg(),
		Limit:      opts.Limit,
		Owner:      opts.Owner,
		Repo:       opts.Repo,
	}
	attestations, err := verification.GetAttestations(c)
	if err != nil {
		if ok := errors.Is(err, api.ErrNoAttestations{}); ok {
			opts.Logger.Printf(opts.Logger.ColorScheme.Red("✗ No attestations found for subject %s\n"), artifact.DigestWithAlg())
			return err
		}

		if c.IsBundleProvided() {
			opts.Logger.Printf(opts.Logger.ColorScheme.Red("✗ Loading attestations from %s failed\n"), artifact.URL)
		} else {
			opts.Logger.Println(opts.Logger.ColorScheme.Red("✗ Loading attestations from GitHub API failed"))
		}
		return err
	}

	pluralAttestation := text.Pluralize(len(attestations), "attestation")
	if c.IsBundleProvided() {
		opts.Logger.Printf("Loaded %s from %s\n", pluralAttestation, opts.BundlePath)
	} else {
		opts.Logger.Printf("Loaded %s from GitHub API\n", pluralAttestation)
	}

	// Apply predicate type filter to returned attestations
	if opts.PredicateType != "" {
		filteredAttestations := verification.FilterAttestations(opts.PredicateType, attestations)

		if len(filteredAttestations) == 0 {
			opts.Logger.Printf(opts.Logger.ColorScheme.Red("✗ No attestations found with predicate type: %s\n"), opts.PredicateType)
			return err
		}

		attestations = filteredAttestations
	}

	policy, err := buildVerifyPolicy(opts, *artifact)
	if err != nil {
		opts.Logger.Println(opts.Logger.ColorScheme.Red("✗ Failed to build verification policy"))
		return err
	}

	sigstoreRes := opts.SigstoreVerifier.Verify(attestations, policy)
	if sigstoreRes.Error != nil {
		opts.Logger.Println(opts.Logger.ColorScheme.Red("✗ Verification failed"))
		return sigstoreRes.Error
	}

	opts.Logger.Println(opts.Logger.ColorScheme.Green("✓ Verification succeeded!\n"))

	// If an exporter is provided with the --json flag, write the results to the terminal in JSON format
	if opts.exporter != nil {
		// print the results to the terminal as an array of JSON objects
		if err = opts.exporter.Write(opts.Logger.IO, sigstoreRes.VerifyResults); err != nil {
			opts.Logger.Println(opts.Logger.ColorScheme.Red("✗ Failed to write JSON output"))
			return err
		}
		return nil
	}

	opts.Logger.Printf("%s was attested by:\n", artifact.DigestWithAlg())

	// Otherwise print the results to the terminal in a table
	tableContent, err := buildTableVerifyContent(sigstoreRes.VerifyResults)
	if err != nil {
		opts.Logger.Println(opts.Logger.ColorScheme.Red("failed to parse results"))
		return err
	}

	headers := []string{"repo", "predicate_type", "workflow"}
	if err = opts.Logger.PrintTable(headers, tableContent); err != nil {
		opts.Logger.Println(opts.Logger.ColorScheme.Red("failed to print attestation details to table"))
		return err
	}

	// All attestations passed verification and policy evaluation
	return nil
}

func extractAttestationDetail(builderSignerURI string) (string, string, error) {
	// If given a build signer URI like
	// https://github.com/foo/bar/.github/workflows/release.yml@refs/heads/main
	// We want to extract:
	// * foo/bar
	// * .github/workflows/release.yml@refs/heads/main
	orgAndRepoRegexp := regexp.MustCompile(`https://github\.com/([^/]+/[^/]+)/`)
	match := orgAndRepoRegexp.FindStringSubmatch(builderSignerURI)
	if len(match) < 2 {
		return "", "", fmt.Errorf("no match found for org and repo")
	}
	repoAndOrg := match[1]

	workflowRegexp := regexp.MustCompile(`https://github\.com/[^/]+/[^/]+/(.+)`)
	match = workflowRegexp.FindStringSubmatch(builderSignerURI)
	if len(match) < 2 {
		return "", "", fmt.Errorf("no match found for workflow")
	}
	workflow := match[1]

	return repoAndOrg, workflow, nil
}

func buildTableVerifyContent(results []*verification.AttestationProcessingResult) ([][]string, error) {
	content := make([][]string, len(results))

	for i, res := range results {
		builderSignerURI := res.VerificationResult.Signature.Certificate.Extensions.BuildSignerURI
		repoAndOrg, workflow, err := extractAttestationDetail(builderSignerURI)
		if err != nil {
			return nil, err
		}
		predicateType := res.VerificationResult.Statement.PredicateType
		content[i] = []string{repoAndOrg, predicateType, workflow}
	}

	return content, nil
}
