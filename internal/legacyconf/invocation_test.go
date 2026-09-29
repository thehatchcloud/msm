package legacyconf

import (
	"errors"
	"slices"
	"testing"

	"github.com/thehatchcloud/msm/internal/serverprops"
)

func invocationSettings(t *testing.T, props string) *ServerSettings {
	t.Helper()
	s, err := ResolveServer(ServerInput{Name: "s", Dir: "/srv/s", Properties: serverprops.Parse([]byte(props))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// CT-SET-039: the default invocation, and placeholders filled per word.
func TestInvocation(t *testing.T) {
	for _, tc := range []struct {
		props string
		want  []string
	}{
		{"", []string{"java", "-Xms1024M", "-Xmx1024M", "-jar", "/srv/s/server.jar", "nogui"}},
		{"msm-ram=512\nmsm-jar-path=/opt/my jars/a.jar\n",
			[]string{"java", "-Xms512M", "-Xmx512M", "-jar", "/opt/my jars/a.jar", "nogui"}},
		{"msm-invocation=  /usr/bin/java\t-Xmx{RAM}M  '-Xlog:gc*:file=gc.log' \"-Dname=a b\" -jar {JAR}  \n",
			[]string{"/usr/bin/java", "-Xmx1024M", "-Xlog:gc*:file=gc.log", "-Dname=a b", "-jar", "/srv/s/server.jar"}},
		{"msm-invocation=java -jar '{JAR}' x''y \"\"\n", []string{"java", "-jar", "/srv/s/server.jar", "xy", ""}},
		// A JAR path is inserted after splitting, so nothing in it is parsed.
		{"msm-jar-path=/j/$(reboot);'x'.jar\n",
			[]string{"java", "-Xms1024M", "-Xmx1024M", "-jar", "/j/$(reboot);'x'.jar", "nogui"}},
	} {
		got, err := invocationSettings(t, tc.props).Invocation()
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("props %q: got %q, %v; want %q", tc.props, got, err, tc.want)
		}
	}
}

// Shell metacharacters are refused, never executed or guessed at.
func TestInvocationRefusesShellSyntax(t *testing.T) {
	for _, inv := range []string{
		"java -jar {JAR}; rm -rf /", "java -jar {JAR} && x", "java | tee", "java > log",
		"java < in", "java $(id)", "java `id`", "java $HOME", `java \x`, "java *.jar",
		"java a?", "java [ab]", "~/java", "java #c", "java !x", "java (x)", "java {SERVER_NAME}",
		"java {RAM", "java }", `java "unterminated`, "java 'unterminated", `java "$HOME"`,
		"java \"a`id`\"", `java "a\b"`, "", "   ", "java\x01",
	} {
		s := invocationSettings(t, "")
		s.raw["INVOCATION"] = inv
		if got, err := s.Invocation(); !errors.Is(err, ErrUnsafeInvocation) {
			t.Errorf("%q: got %q, %v", inv, got, err)
		}
	}
	for _, ram := range []string{"0", "-1", "1g", "", "99999999999"} {
		s := invocationSettings(t, "msm-ram="+ram+"\n")
		if ram == "" {
			s.values["RAM"] = ""
		}
		if got, err := s.Invocation(); !errors.Is(err, ErrUnsafeInvocation) {
			t.Errorf("RAM %q: got %q, %v", ram, got, err)
		}
	}
}
