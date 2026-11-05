package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/auth"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/config"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
)

// runCreateAdmin creates a platform administrator (plan 14: the first admin is
// created only through studioctl). It connects as studio_app (plan 6.2) via
// STUDIO_DATABASE_URL and reads the password from stdin, never from flags or
// the environment, so it does not appear in shell history or process lists.
func runCreateAdmin(ctx context.Context, args []string, lookup config.Lookup, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("create-admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	email := fs.String("email", "", "administrator email (required)")
	name := fs.String("name", "", "display name (required)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *email == "" || *name == "" {
		_, _ = fmt.Fprintln(stderr, "usage: studioctl create-admin --email <address> --name <display name> < password-file")
		return 2
	}
	dbURL, err := requireURL(lookup, config.EnvDatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	password, err := readPassword(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "password: %v\n", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := db.Open(ctx, dbURL, "studioctl")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer pool.Close()

	osUser := ""
	if u, err := user.Current(); err == nil {
		osUser = u.Username
	}
	host, _ := os.Hostname()
	id, err := auth.NewService(pool, auth.Config{}).CreateAdmin(ctx, auth.AdminInput{
		Email: *email, DisplayName: *name, Password: password, OSUser: osUser, Hostname: host,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "create-admin: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "created platform administrator %s (principal %s)\n", strings.ToLower(strings.TrimSpace(*email)), id)
	return 0
}

// readPassword reads one line from stdin, without its line ending.
func readPassword(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("no stdin")
	}
	line, err := bufio.NewReaderSize(r, auth.MaxPasswordLength+2).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if err := auth.ValidatePassword(pw); err != nil {
		return "", err
	}
	return pw, nil
}
