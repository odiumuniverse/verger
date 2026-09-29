package service

import (
	"encoding/xml"
	"errors"
	"path/filepath"
	"strings"
)

// RenderLaunchd renders a launchd agent for a login service: RunAtLoad so it
// starts at login, KeepAlive so it comes back, a throttle so a crash loop does
// not spin, and a log path under the store.
func RenderLaunchd(spec Spec) (string, string, error) {
	if spec.Binary == "" {
		return "", "", errors.New("service: a launchd agent needs the absolute path to the binary")
	}

	path, err := UnitPath(spec.Home, spec.LabelOrDefault())
	if err != nil {
		return "", "", err
	}

	args := append([]string{spec.Binary}, spec.ArgsOrDefault()...)

	var b strings.Builder

	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")

	writePlistString(&b, "Label", spec.LabelOrDefault())
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")

	for _, arg := range args {
		b.WriteString("\t\t<string>" + xmlEscape(arg) + "</string>\n")
	}

	b.WriteString("\t</array>\n")
	// RunAtLoad starts it at login; KeepAlive is what makes a watcher that
	// dies come back. ThrottleInterval keeps a crash loop from spinning.
	b.WriteString("\t<key>RunAtLoad</key><true/>\n")
	b.WriteString("\t<key>KeepAlive</key><true/>\n")
	b.WriteString("\t<key>ThrottleInterval</key><integer>10</integer>\n")
	b.WriteString("\t<key>ProcessType</key><string>Background</string>\n")
	b.WriteString("\t<key>SoftResourceLimits</key><dict><key>NumberOfFiles</key><integer>" +
		fileLimit(spec) + "</integer></dict>\n")

	if len(spec.Env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")

		for _, pair := range EnvPairs(spec.Env) {
			b.WriteString("\t\t<key>" + xmlEscape(pair[0]) + "</key><string>" + xmlEscape(pair[1]) + "</string>\n")
		}

		b.WriteString("\t</dict>\n")
	}

	if spec.LogPath != "" {
		writePlistString(&b, "StandardOutPath", spec.LogPath)
	}

	if spec.ErrLogPath != "" {
		writePlistString(&b, "StandardErrorPath", spec.ErrLogPath)
	}

	b.WriteString("</dict>\n</plist>\n")

	return path, b.String(), nil
}

// RenderSystemd renders a systemd --user unit. It is a --user unit on purpose:
// verger watches one user's store, and a root unit would watch the wrong one
// and need a password to install.
func RenderSystemd(spec Spec) (string, string, error) {
	if spec.Binary == "" {
		return "", "", errors.New("service: a systemd unit needs the absolute path to the binary")
	}

	path, err := UnitPath(spec.Home, spec.LabelOrDefault())
	if err != nil {
		return "", "", err
	}

	args := append([]string{spec.Binary}, spec.ArgsOrDefault()...)

	var b strings.Builder

	b.WriteString("[Unit]\n")
	b.WriteString("Description=verger watch\n")
	b.WriteString("After=default.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + systemdCommand(args) + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=2\n")
	// NoNewPrivileges is the one hardening line that costs a watcher nothing;
	// the watcher reads the user's own files and never needs to be root.
	b.WriteString("NoNewPrivileges=yes\n")
	b.WriteString("ProtectSystem=strict\n")
	b.WriteString("ProtectHome=no\n")
	b.WriteString("MemoryMax=128M\n")
	b.WriteString("StandardOutput=journal\n")
	b.WriteString("StandardError=journal\n")

	for _, pair := range EnvPairs(spec.Env) {
		b.WriteString("Environment=" + systemdEnv(pair[0], pair[1]) + "\n")
	}

	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")

	return path, b.String(), nil
}

// writePlistString writes one <key>/<string> pair into a plist dict.
func writePlistString(b *strings.Builder, key, value string) {
	b.WriteString("\t<key>" + xmlEscape(key) + "</key><string>" + xmlEscape(value) + "</string>\n")
}

// xmlEscape escapes the four characters that are special inside plist text.
func xmlEscape(s string) string {
	var b strings.Builder

	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		// EscapeText only fails on a writer that fails, and strings.Builder
		// cannot. Returning the input unescaped would be worse than the
		// panic, so hand the caller something readable.
		return s
	}

	return b.String()
}

// unitName turns a label into a systemd unit file name.
func unitName(label string) string {
	return label + ".service"
}

// systemdCommand renders argv as an ExecStart line, quoting each argument so a
// path with a space survives.
func systemdCommand(args []string) string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		out = append(out, systemdQuote(arg))
	}

	return strings.Join(out, " ")
}

// systemdQuote quotes one ExecStart argument. systemd splits on whitespace
// unless the argument is quoted, so an unquoted path with a space would become
// two arguments and the service would fail to start for a reason that is
// invisible in the unit file.
func systemdQuote(arg string) string {
	if arg == "" {
		return `""`
	}

	if !strings.ContainsAny(arg, " \t\"'\\$") {
		return arg
	}

	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`).Replace(arg) + `"`
}

// systemdEnv renders one Environment= line with the same quoting rule.
func systemdEnv(key, value string) string {
	return key + "=" + systemdQuote(value)
}

// binaryFrom returns the first ProgramArguments entry of a rendered launchd
// plist, or the first ExecStart token of a rendered systemd unit. Reading the
// path out of the file is the point: the question "did the binary move" is
// about what is installed, not about what a caller would install today.
func binaryFrom(content string) string {
	if strings.Contains(content, "<key>ProgramArguments</key>") {
		// The first <string> in a plist is the Label, which comes before
		// ProgramArguments. The program is the first <string> *after* the
		// arguments array opens.
		_, after, found := strings.Cut(content, "<key>ProgramArguments</key>")
		if !found {
			return ""
		}

		_, after, found = strings.Cut(after, "<array>")
		if !found {
			return ""
		}

		return firstBetween(after, "<string>", "</string>")
	}

	if _, after, found := strings.Cut(content, "ExecStart="); found {
		line, _, _ := strings.Cut(after, "\n")

		fields := strings.Fields(line)
		if len(fields) == 0 {
			return ""
		}

		return strings.Trim(fields[0], `"`)
	}

	return ""
}

// firstBetween returns the text between the first open and closing marker.
// The closing parameter is `closer`, not `close`, so it does not shadow the
// predeclared identifier of the same name.
func firstBetween(s, open, closer string) string {
	_, after, found := strings.Cut(s, open)
	if !found {
		return ""
	}

	value, _, found := strings.Cut(after, closer)
	if !found {
		return ""
	}

	return value
}

// storeLogs returns the log paths a spec should use: both under the store, so
// a service never writes a log outside the tree the user can see and eject.
func storeLogs(home, vergerRoot string) (string, string) {
	root := vergerRoot
	if root == "" {
		root = filepath.Join(home, ".verger")
	}

	return filepath.Join(root, "logs", "watch.log"), filepath.Join(root, "logs", "watch.err.log")
}
