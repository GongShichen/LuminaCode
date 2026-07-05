package skills

import (
	"fmt"
	"strings"

	appsecurity "LuminaCode/security"
	bashpkg "LuminaCode/tools/bash"
)

func RunShellSafetyChecks(command, cwd string) error {
	securityResult := bashpkg.RunAllSecurityChecks(command)
	if bashpkg.IsBlocking(securityResult) {
		descriptions := make([]string, 0, len(securityResult.Findings))
		for i, finding := range securityResult.Findings {
			if i >= 3 {
				break
			}
			descriptions = append(descriptions, finding.Description)
		}
		return fmt.Errorf("Inline shell blocked by security checks: %s", strings.Join(descriptions, "; "))
	}
	if appsecurity.IsDangerous(command) {
		return fmt.Errorf("Inline shell command matches a dangerous pattern.")
	}
	valid, invalidPaths := bashpkg.ValidatePaths(command, cwd)
	if !valid {
		if len(invalidPaths) > 5 {
			invalidPaths = invalidPaths[:5]
		}
		return fmt.Errorf("Inline shell command references paths outside the workspace: %s", strings.Join(invalidPaths, ", "))
	}
	return nil
}
