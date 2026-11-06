package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Tools runs pg_dump and pg_restore. Plan 13 requires the PostgreSQL 18
// tools from the database container; the host's pg_dump (version 14) is
// never used.
type Tools interface {
	// Version returns the pg_dump version string.
	Version(ctx context.Context) (string, error)
	// Dump writes pg_dump -Fc of snapshot in the database of conn to out.
	Dump(ctx context.Context, conn Conn, snapshot string, out io.Writer) error
	// Restore runs pg_restore of in into the database of conn.
	Restore(ctx context.Context, conn Conn, in io.Reader) error
}

// Conn names a role login to one database as seen from inside the
// container. The password is passed only through the environment.
type Conn struct {
	User     string
	Password string
	Database string
}

// ConnFromURL extracts the user, password and database of a postgres URL.
func ConnFromURL(raw string) (Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Conn{}, errors.New("backup: invalid database URL")
	}
	pw, _ := u.User.Password()
	c := Conn{User: u.User.Username(), Password: pw, Database: strings.TrimPrefix(u.Path, "/")}
	if c.User == "" || c.Database == "" {
		return Conn{}, errors.New("backup: database URL needs a user and a database name")
	}
	return c, nil
}

// DockerTools runs the tools with `docker exec` in the PostgreSQL container,
// connecting to the server inside it over TCP on 127.0.0.1:5432.
type DockerTools struct {
	Container string
}

var _ Tools = DockerTools{}

var (
	majorRE     = regexp.MustCompile(`\(PostgreSQL\) (\d+)\.`)
	containerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
)

// Version implements Tools and refuses any pg_dump other than version 18.
func (d DockerTools) Version(ctx context.Context) (string, error) {
	var out bytes.Buffer
	if err := d.run(ctx, Conn{}, []string{"pg_dump", "--version"}, nil, &out); err != nil {
		return "", err
	}
	v := strings.TrimSpace(out.String())
	m := majorRE.FindStringSubmatch(v)
	if m == nil || m[1] != "18" {
		return "", fmt.Errorf("backup: the container's pg_dump is %q; PostgreSQL 18 is required", v)
	}
	return v, nil
}

// Dump implements Tools.
func (d DockerTools) Dump(ctx context.Context, c Conn, snapshot string, out io.Writer) error {
	return d.run(ctx, c, []string{
		"pg_dump", "--format=custom", "--snapshot=" + snapshot, "--no-password",
		"--host=127.0.0.1", "--port=5432", "--username=" + c.User, "--dbname=" + c.Database,
	}, nil, out)
}

// Restore implements Tools with the flags of plan 13, step 3.
func (d DockerTools) Restore(ctx context.Context, c Conn, in io.Reader) error {
	return d.run(ctx, c, []string{
		"pg_restore", "--no-owner", "--role=studio_owner", "--exit-on-error", "--single-transaction", "--no-password",
		"--host=127.0.0.1", "--port=5432", "--username=" + c.User, "--dbname=" + c.Database,
	}, in, io.Discard)
}

func (d DockerTools) run(ctx context.Context, c Conn, cmdline []string, in io.Reader, out io.Writer) error {
	if !containerRE.MatchString(d.Container) {
		return errors.New("backup: the PostgreSQL container must be a docker container name or id")
	}
	// "-e PGPASSWORD" with no value copies it from this process's
	// environment, so the password never appears in any argument list.
	args := []string{"exec", "-i", "-e", "PGPASSWORD", d.Container}
	args = append(args, cmdline...)
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // G204: fixed program; arguments are the tool flags above and validated names.
	cmd.Env = append(os.Environ(), "PGPASSWORD="+c.Password)
	cmd.Stdin = in
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[:2000]
		}
		return fmt.Errorf("backup: %s failed: %w: %s", cmdline[0], err, msg)
	}
	return nil
}
