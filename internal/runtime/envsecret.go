package runtime

// envArgvSafe is deliberately an allow list: unknown non-empty environment
// values are treated as credentials and must not be passed through argv.
var envArgvSafe = map[string]bool{
	"COLORTERM": true, "LANG": true, "LANGUAGE": true, "LC_ALL": true,
	"LC_COLLATE": true, "LC_CTYPE": true, "LC_MESSAGES": true,
	"LC_NUMERIC": true, "LC_TIME": true, "TERM": true, "TZ": true,
	"GC_AGENT": true, "GC_ALIAS": true, "GC_CITY": true, "GC_CITY_PATH": true,
	"GC_PROVIDER": true, "GC_RIG": true, "GC_RIG_ROOT": true, "GC_TEMPLATE": true,
	"GT_CREW": true, "GT_RIG": true, "GT_ROLE": true, "GT_PROCESS_NAMES": true,
	"GC_CONTINUATION_EPOCH": true, "GC_READY_PROMPT_PREFIX": true,
	"GC_RUNTIME_EPOCH": true, "GC_SESSION_ID": true, "GC_SESSION_NAME": true,
	"GC_SESSION_ORIGIN": true, "BEADS_DIR": true, "GC_AGENT_SLICE": true,
	"GC_BIN": true, "GC_BLESSED_BIN_DIR": true, "GC_DIR": true,
	"GC_DOLT_PORT": true, "GC_HOME": true, "GC_SKILLS_DIR": true,
}

// ArgvSafeEnvKey reports whether key has been reviewed as safe for argv.
func ArgvSafeEnvKey(key string) bool { return envArgvSafe[key] }

// ArgvSecretEnvValue reports whether a non-empty environment value must be
// withheld from process argument vectors.
func ArgvSecretEnvValue(key, value string) bool {
	return value != "" && !ArgvSafeEnvKey(key)
}

// EnvHasArgvSecrets reports whether env contains a value that requires private
// transport rather than argv.
func EnvHasArgvSecrets(env map[string]string) bool {
	for key, value := range env {
		if ArgvSecretEnvValue(key, value) {
			return true
		}
	}
	return false
}

// SplitEnvByArgvSafety partitions env into entries that may travel in argv and
// entries that must use a private transport. Both results are non-nil.
func SplitEnvByArgvSafety(env map[string]string) (safe, secret map[string]string) {
	safe = make(map[string]string, len(env))
	secret = make(map[string]string)
	for key, value := range env {
		if ArgvSecretEnvValue(key, value) {
			secret[key] = value
			continue
		}
		safe[key] = value
	}
	return safe, secret
}
