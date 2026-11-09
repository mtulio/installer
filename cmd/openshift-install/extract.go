package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/openshift/installer/pkg/clusterapi"
)

var extractOpts struct {
	destDir string
}

// newExtractCmd returns the hidden `extract` command, which writes artifacts
// the installer embeds out to the filesystem.
//
// It is hidden because it exposes an implementation detail -- which Cluster
// API providers this binary happens to embed -- and because the artifacts it
// writes are only consumable by the External platform, which is itself not a
// supported configuration yet.
func newExtractCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "extract",
		Short:  "Extract artifacts embedded in the installer",
		Long:   "",
		Hidden: true,
	}
	cmd.AddCommand(newExtractClusterAPICmd())
	return cmd
}

func newExtractClusterAPICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster-api PROVIDER",
		Short: "Extract a Cluster API infrastructure provider",
		Long: fmt.Sprintf(`Write an embedded Cluster API infrastructure provider's controller
binary and component manifests to a directory.

The extracted artifacts are the inputs to a provider payload image, so this
makes it possible to exercise the External platform with a provider this
installer builds and tests itself, rather than with an unknown external build.
The install-config points at a payload image, never at these paths.

Known providers: %s`, knownProviders()),
		// The root command silences usage and errors (main.go:79-80), so
		// cobra.ExactArgs would surface only "accepts 1 arg(s), received 0"
		// with no hint of what a provider name looks like. The whole point of
		// this command is that the caller may not know what is embedded.
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("expected exactly one provider name, got %d: known providers are %s",
					len(args), knownProviders())
			}
			return nil
		},
		RunE: func(_ *cobra.Command, args []string) error {
			if extractOpts.destDir == "" {
				return fmt.Errorf("--dest-dir is required: it names the directory to write the artifacts to")
			}
			extracted, err := clusterapi.ExtractProvider(args[0], extractOpts.destDir)
			if err != nil {
				return err
			}
			fmt.Printf("\nBuild a provider payload image around these artifacts. "+
				"A descriptor to start from:\n\n%s\n", extracted.PayloadDescriptor())
			return nil
		},
	}
	cmd.Flags().StringVar(&extractOpts.destDir, "dest-dir", "", "directory to write the artifacts to")
	return cmd
}

func knownProviders() string {
	return strings.Join(clusterapi.InfrastructureProviderNames(), ", ")
}
