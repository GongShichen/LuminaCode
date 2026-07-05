package security

import (
	"regexp"
	"strings"
)

var dangerousPatterns = []*regexp.Regexp{
	// File destruction
	regexp.MustCompile(`\brm\s`),
	regexp.MustCompile(`\brmdir\s`),

	// Git destructive operations
	regexp.MustCompile(`\bgit\s+(?:push|reset|clean|checkout\s+\.)`),
	regexp.MustCompile(`(?i)\bgit\s+push\s+.*(?:--force|-f\b)`),
	regexp.MustCompile(`\bgit\s+commit\s+--amend`),

	// Privilege escalation
	regexp.MustCompile(`\bsudo\b`),
	regexp.MustCompile(`\bdoas\b`),
	regexp.MustCompile(`\bpkexec\b`),
	regexp.MustCompile(`\bsu\s`),

	// Filesystem destruction
	regexp.MustCompile(`\bmkfs\b`),
	regexp.MustCompile(`\bdd\s`),
	regexp.MustCompile(`>\s*/dev/(?:sd|hd|nvme|mmcblk)`),

	// Process termination
	regexp.MustCompile(`\bkill\b`),
	regexp.MustCompile(`\bpkill\b`),
	regexp.MustCompile(`\bkillall\b`),

	// System shutdown/reboot
	regexp.MustCompile(`\breboot\b`),
	regexp.MustCompile(`\bshutdown\b`),
	regexp.MustCompile(`\bhalt\b`),
	regexp.MustCompile(`\bpoweroff\b`),

	// Permission changes
	regexp.MustCompile(`\bchmod\s+777`),
	regexp.MustCompile(`\bchmod\s+.*777`),
	regexp.MustCompile(`\bchown\s`),
	regexp.MustCompile(`\bchattr\s`),

	// Fork bomb detection
	regexp.MustCompile(`\bfork\s+bomb|:\s*\(\)\s*\{`),

	// Pipe to shell
	regexp.MustCompile(`\bwget\s.*\|\s*(?:ba)?sh`),
	regexp.MustCompile(`\bcurl\s.*\|\s*(?:ba)?sh`),

	// Network data exfiltration
	regexp.MustCompile(`\bnc\s+.*-e\s`),
	regexp.MustCompile(`\bncat\s+.*-e\s`),

	// Disk/device manipulation
	regexp.MustCompile(`\bfdisk\b`),
	regexp.MustCompile(`\bparted\b`),
	regexp.MustCompile(`\bmount\s`),
	regexp.MustCompile(`\bumount\s`),

	// Kernel module manipulation
	regexp.MustCompile(`\bmodprobe\b`),
	regexp.MustCompile(`\binsmod\b`),
	regexp.MustCompile(`\brmmod\b`),

	// System configuration changes
	regexp.MustCompile(`\bsysctl\s`),
	regexp.MustCompile(`\biptables\b`),
	regexp.MustCompile(`\bnft\b`),
	regexp.MustCompile(`\bufw\s`),

	// Service manipulation
	regexp.MustCompile(`\bsystemctl\s+(?:start|stop|restart|enable|disable|mask)`),
	regexp.MustCompile(`\bservice\s+\w+\s+(?:start|stop|restart)`),

	// Cron/scheduled task manipulation
	regexp.MustCompile(`\bcrontab\s`),

	// User account manipulation
	regexp.MustCompile(`\buseradd\b`),
	regexp.MustCompile(`\buserdel\b`),
	regexp.MustCompile(`\busermod\b`),
	regexp.MustCompile(`\bpasswd\b`),

	// Dangerous find -exec
	regexp.MustCompile(`\bfind\s.*-exec\b`),

	// Dangerous xargs
	regexp.MustCompile(`\bxargs\s.*\brm\b`),
	regexp.MustCompile(`\bxargs\s.*\bsh\b`),

	// Environment variable injection
	regexp.MustCompile(`\bLD_PRELOAD\s*=`),
	regexp.MustCompile(`\bLD_LIBRARY_PATH\s*=`),

	// Reverse shells
	regexp.MustCompile(`\b(?:nc|ncat|netcat)\s+.*(?:-e|--exec)\s+(?:/bin/|/usr/bin/)?(?:ba)?sh`),
	regexp.MustCompile(`\b(?:python|python3|ruby|perl|php)\s+.*(?:socket|subprocess|exec)`),
}

func IsDangerous(command string) bool {
	for _, re := range dangerousPatterns {
		if re.MatchString(command) {
			return true
		}
	}
	return false
}

type RiskLevel string

const (
	RiskLevelSafe      RiskLevel = "safe"
	RiskLevelWarning   RiskLevel = "warning"
	RiskLevelDangerous RiskLevel = "dangerous"
	RiskLevelCritical  RiskLevel = "critical"
)

func AssessRisk(command string) RiskLevel {
	if strings.TrimSpace(command) == "" {
		return RiskLevelSafe
	}

	criticalPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\brm\s+.*-rf\s+/`),
		regexp.MustCompile(`\brm\s+.*-rf\s+~`),
		regexp.MustCompile(`:\s*\(\)\s*\{`),
		regexp.MustCompile(`\bsudo\s+rm\s+.*-rf\s+/`),
	}
	for _, p := range criticalPatterns {
		if p.MatchString(command) {
			return RiskLevelCritical
		}
	}

	if IsDangerous(command) {
		return RiskLevelDangerous
	}

	warningPatterns := []*regexp.Regexp{
		regexp.MustCompile(`>\s*\w`),
		regexp.MustCompile(`\b(?:mv|cp|touch|mkdir)\s`),
		regexp.MustCompile(`\bgit\s+(?:commit|branch|tag|merge|rebase)`),
		regexp.MustCompile(`\b(?:npm|yarn|pnpm|pip|pip3|cargo)\s+(?:install|add|remove|update)`),
	}
	for _, p := range warningPatterns {
		if p.MatchString(command) {
			return RiskLevelWarning
		}
	}
	return RiskLevelSafe
}
