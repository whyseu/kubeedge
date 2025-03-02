/*
Copyright 2022 The KubeEdge Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package beta

import (
	"github.com/spf13/cobra"

	edge "github.com/kubeedge/kubeedge/keadm/cmd/keadm/app/cmd/edge"
)

// NewBeta represents the beta command
func NewBeta() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "beta",
		Short: "keadm beta command",
		Long:  `keadm beta command provides some subcommands that are still in testing, but have complete functions and can be used in advance`,
	}

	cmd.ResetFlags()

	cmd.AddCommand(edge.NewJoinBetaCommand())
	cmd.AddCommand(NewBetaInit())
	cmd.AddCommand(NewBetaManifestGenerate())
	cmd.AddCommand(newCmdConfig())
	cmd.AddCommand(NewKubeEdgeResetBeta())

	return cmd
}
