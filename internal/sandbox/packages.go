package sandbox

import (
	"context"
	"fmt"
	"strings"
)

type packageSetupCommand struct {
	manager string
	command Command
}

// packageSetupSandbox separates trusted provisioning from untrusted agent
// commands when a provider supports per-command identities. Implementations
// must still execute inside the sandbox boundary.
type trustedPackageSetupSandbox interface {
	ExecPackageSetup(context.Context, Command) (*Result, error)
}

func packageSetupCommands(packages PackageSet) []packageSetupCommand {
	commands := make([]packageSetupCommand, 0, 7)
	if len(packages.Apt) > 0 {
		commands = append(commands,
			packageSetupCommand{manager: "apt", command: Command{Path: "apt-get", Args: []string{"update"}}},
			packageSetupCommand{manager: "apt", command: Command{
				Path: "apt-get", Args: append([]string{"install", "-y", "--no-install-recommends"}, packages.Apt...),
			}},
		)
	}
	commands = appendPackageCommand(commands, "cargo", "cargo", []string{"install"}, packages.Cargo)
	commands = appendPackageCommand(commands, "gem", "gem", []string{"install"}, packages.Gem)
	commands = appendPackageCommand(commands, "go", "go", []string{"install"}, packages.Go)
	commands = appendPackageCommand(commands, "npm", "npm", []string{"install", "--global"}, packages.NPM)
	commands = appendPackageCommand(commands, "pip", "python3", []string{"-m", "pip", "install"}, packages.Pip)
	return commands
}

func appendPackageCommand(
	commands []packageSetupCommand,
	manager string,
	path string,
	prefix []string,
	packages []string,
) []packageSetupCommand {
	if len(packages) == 0 {
		return commands
	}
	return append(commands, packageSetupCommand{
		manager: manager,
		command: Command{Path: path, Args: append(append([]string(nil), prefix...), packages...)},
	})
}

func initializeSandbox(ctx context.Context, box Sandbox, spec Spec) error {
	execute := box.Exec
	if setupBox, ok := box.(trustedPackageSetupSandbox); ok {
		execute = setupBox.ExecPackageSetup
	}
	for _, setup := range packageSetupCommands(spec.Packages) {
		result, err := execute(ctx, setup.command)
		if err != nil {
			return fmt.Errorf("sandbox: install %s packages: %w", setup.manager, err)
		}
		if result == nil {
			return fmt.Errorf("sandbox: install %s packages: empty execution result", setup.manager)
		}
		if result.ExitCode != 0 {
			detail := strings.TrimSpace(string(result.Stderr))
			if detail == "" {
				detail = strings.TrimSpace(string(result.Stdout))
			}
			if detail == "" {
				detail = "package manager exited unsuccessfully"
			}
			return fmt.Errorf(
				"sandbox: install %s packages: exit %d: %s",
				setup.manager,
				result.ExitCode,
				detail,
			)
		}
	}
	return nil
}
