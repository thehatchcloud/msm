package cli

import "sort"

// helpTopics are the subjects `msm help <topic>` explains beyond the
// legacy command list. They are kept in step with docs/cli.md.
var helpTopics = map[string]string{
	"targets": `Targets

A server command is "msm <server> <command>". In place of a server, "all"
runs the command on every server, one report per server, up to --jobs at a
time for start, stop and restart. Every command that takes a server accepts
"all" except console and cmdlog, which need exactly one.

"msm all stop" marks every server inactive, as "msm <server> stop" does;
the global "msm stop" stops them all and leaves them marked active.
`,
	"formats": `Output formats

Ordinary output is text for people. --format json prints a versioned JSON
document instead, for: version, config, server list, <server|all> status and
<server> config (the listing). Warnings and errors always go to standard
error. Every document has "schema_version"; a field is never removed or
changed in meaning without incrementing it.
`,
	"exit-codes": `Exit codes

  0    ok            the command did what was asked
  1    error         the command failed, or some server of a bulk command did
  2    usage         no such command, a wrong argument count, or a bad flag
  3    config        a configuration file, setting or value cannot be used
  4    not-found     the server, world, JAR group or property does not exist
  5    refused       declined for safety: confirmation missing, server not
                     proven stopped, name taken, or a lock is held
  6    unavailable   a program or feature needed (such as GNU screen) is
                     missing, or this build cannot do it (update in a development build)
  130  interrupted   stopped by Ctrl+C or a termination request

A refusal (5) and a usage or configuration error (2, 3) change nothing.
`,
	"noninteractive": `Noninteractive use

msm never waits for input that cannot come. When standard input is not a
terminal, a destructive command (server delete, jargroup delete) refuses
with exit status 5 unless you pass --yes, after printing what it would
remove. Nothing else asks a question.
`,
}

func helpTopicNames() []string {
	names := make([]string, 0, len(helpTopics))
	for n := range helpTopics {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
