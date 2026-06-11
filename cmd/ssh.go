package cmd

import (
	"fmt"
	"os"

	"github.com/janvete/caddyview/internal/sshclient"
	"github.com/janvete/caddyview/internal/tui"
	"github.com/spf13/cobra"
)

var (
	sshPort    int
	logPath    string
	sshKeyPath string
)

var sshCmd = &cobra.Command{
	Use:   "ssh [user@host]",
	Short: "Connect to server and monitor Caddy logs",
	Example: `  caddyview ssh root@192.168.1.1
  caddyview ssh -p 2222 admin@myserver.com
  caddyview ssh -l /var/log/caddy/access.log root@192.168.1.1`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]

		client, err := sshclient.Connect(target, sshPort, sshKeyPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "SSH connection failed: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()

		return tui.Run(client, logPath)
	},
}

func init() {
	rootCmd.AddCommand(sshCmd)
	sshCmd.Flags().IntVarP(&sshPort, "port", "p", 22, "SSH port")
	sshCmd.Flags().StringVarP(&logPath, "log", "l", "/var/log/caddy/access.log", "Path to Caddy access log on remote server")
	sshCmd.Flags().StringVarP(&sshKeyPath, "key", "i", "", "SSH private key path (default: ~/.ssh/id_rsa or id_ed25519)")
}
