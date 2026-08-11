package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"dmdul/internal/dm"
	"dmdul/internal/version"
)

const usageText = `dmdul - Dameng Database Offline Recovery & Data Unloader

Usage:
  dmdul
  dmdul help
  dmdul version

Run dmdul without arguments to enter the interactive shell.

Main interactive commands:
  bootstrap;
  load dictionary;
  list datafile;
  list asmfile;
  cp <+GROUP/path/file> <filesystem file|directory>;
  cp datafile <filesystem directory>;
  list user;
  list schema [owner];
  list table <owner>;
  describe <owner.table_name>;
  unload table <owner.table_name>;
  unload object <owner|all>;
  unload user <owner>;
  unload schema <schema>[,<schema>...];
  unload database;
  recover table <owner.table_name>;
  check pages [<dbf-name>[,<dbf-name>...]] [control];
  set asm_disk <raw member>[,<raw member>...];
  show parameter;
  help;
  exit;
`

const (
	defaultSystemPath     = "SYSTEM.DBF"
	defaultControlDULPath = "control.dul"
	defaultInitDULPath    = "init.dul"
	defaultOutputDirName  = "output"
)

var removedFunctionalCommands = map[string]bool{
	"inspect":         true,
	"inspect-ctl":     true,
	"scan-system":     true,
	"scan-partitions": true,
	"export-ddl":      true,
	"export-data":     true,
}

func Run(args []string, stdout io.Writer, stderr io.Writer) error {
	if len(args) == 0 {
		return RunInteractive(os.Stdin, stdout, stderr)
	}

	command := strings.ToLower(strings.TrimSpace(args[0]))
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return nil
	case "version":
		fmt.Fprintln(stdout, version.String())
		return nil
	default:
		if removedFunctionalCommands[command] {
			return fmt.Errorf("command %q has been removed; run dmdul without arguments and use the interactive shell", args[0])
		}
		return fmt.Errorf("unknown command %q; run dmdul without arguments to enter the interactive shell\n\n%s", args[0], usageText)
	}
}

func defaultIfBlank(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func optionalControlPathForSystem(systemPath string, ctlPath string, ctlProvided bool) (string, bool) {
	if strings.TrimSpace(ctlPath) != "" {
		return ctlPath, ctlProvided
	}
	if ctlProvided {
		return "", true
	}
	defaultCtlPath := dm.DefaultControlPathForSystem(systemPath)
	if err := validateRegularFile(defaultCtlPath); err == nil {
		return defaultCtlPath, false
	}
	return "", false
}

func validateOptionalControlInputFiles(command string, systemPath string, ctlPath string, ctlProvided bool) error {
	if err := validateRegularFile(systemPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s requires SYSTEM.DBF; file not found: %s", command, systemPath)
		}
		return fmt.Errorf("%s cannot access SYSTEM.DBF %q: %w", command, systemPath, err)
	}
	if strings.TrimSpace(ctlPath) == "" {
		return nil
	}
	if err := validateRegularFile(ctlPath); err != nil {
		if os.IsNotExist(err) && !ctlProvided {
			return nil
		}
		if os.IsNotExist(err) {
			return fmt.Errorf("%s cannot access dm.ctl %q: file does not exist", command, ctlPath)
		}
		return fmt.Errorf("%s cannot access dm.ctl %q: %w", command, ctlPath, err)
	}
	return nil
}

func validateRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	return nil
}

func truncateForTable(value string, width int) string {
	if width <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}
