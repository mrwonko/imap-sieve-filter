package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"iter"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

var (
	// Header fields to download.
	// Add any you want to filter by.
	headerFields = []string{"Subject", "From"}
)

/*
var c *imapclient.Client

seqSet := imap.SeqSetNum(1)
bodySection := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader}
fetchOptions := &imap.FetchOptions{
	Flags:       true,
	Envelope:    true,
	BodySection: []*imap.FetchItemBodySection{bodySection},
}
messages, err := c.Fetch(seqSet, fetchOptions).Collect()
if err != nil {
	log.Fatalf("FETCH command failed: %v", err)
}

msg := messages[0]
header := msg.FindBodySection(bodySection)

log.Printf("Flags: %v", msg.Flags)
log.Printf("Subject: %v", msg.Envelope.Subject)
log.Printf("Header:\n%v", string(header))
*/

func main() {
	if err := doMain(); err != nil {
		log.Fatal(err)
	}
}

func doMain() error {
	var (
		host, user, mailboxName, script, sieveConfig string
		execute                                      bool
	)
	flag.StringVar(&host, "host", "", "domain and port to connect to, e.g. mail.example.org:993")
	flag.StringVar(&user, "user", "", "user name, e.g. user@mail.example.org")
	flag.StringVar(&script, "script", "", "Sieve script to execute")
	flag.StringVar(&mailboxName, "mailbox", "", "Mailbox to filter, e.g. INBOX")
	flag.StringVar(&sieveConfig, "sieve-config", "", "Optional config file to pass to sieve-test via -c")
	flag.BoolVar(&execute, "execute", false, "Disables dry-run mode and actually performs actions.")
	flag.Parse()
	password := os.Getenv("IMAP_PASS")

	if host == "" || user == "" || mailboxName == "" {
		flag.Usage()
		return nil
	}
	if password == "" {
		return errors.New("please provide the password using environment variable IMAP_PASS")
	}

	if !execute {
		log.Print("Dry-run mode enabled. Use -execute to disable it.")
	}

	log.Printf("Connecting to %s", host)
	c, err := imapclient.DialTLS(host, nil)
	if err != nil {
		return fmt.Errorf("failed to dial IMAP server: %w", err)
	}
	defer c.Close()

	log.Printf("Performing login")
	if err := c.Login(user, password).Wait(); err != nil {
		return fmt.Errorf("failed to login: %w", err)
	}

	log.Printf("Selecting mailbox")
	mailbox, err := c.Select(mailboxName, &imap.SelectOptions{ReadOnly: !execute}).Wait()
	if err != nil {
		return fmt.Errorf("failed to select mailbox: %w", err)
	}
	tmpfile, err := os.CreateTemp("", "sieve-test-input-*.eml")
	if err != nil {
		return fmt.Errorf("failed to create temporary file for sieve-test input: %w", err)
	}
	defer func() {
		filename := tmpfile.Name()
		if err := os.Remove(filename); err != nil {
			log.Printf("failed to delete temporary email file %q: %s", filename, err)
		}
	}()
	log.Printf("%d messages in mailbox %s", mailbox.NumMessages, mailboxName)

	var sieveArgs []string
	if sieveConfig != "" {
		sieveArgs = append(sieveArgs, "-c", sieveConfig)
	}
	sieveArgs = append(sieveArgs, script, tmpfile.Name())
	{
		quotedSieveArgs := make([]string, len(sieveArgs))
		for i, arg := range sieveArgs {
			quotedSieveArgs[i] = fmt.Sprintf("%q", arg)
		}
		log.Printf("prepared command: sieve-test %s", strings.Join(quotedSieveArgs, " "))
	}

	var stdoutBuf, stderrBuf bytes.Buffer // outside of loop to re-use the memory
	movesByDestination := map[string]imap.UIDSet{}

	bodySection := &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: headerFields,
	}
	messages := c.Fetch(imap.SeqSet{{Start: 1, Stop: 0}}, &imap.FetchOptions{
		UID:         true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	})
	for msgFuture, err := range allAndClose(messages) {
		if err != nil {
			return fmt.Errorf("failed to fetch mail: %w", err)
		}
		msg, err := msgFuture.Collect()
		if err != nil {
			return fmt.Errorf("failed to download message %d/%d: %w", msgFuture.SeqNum, mailbox.NumMessages, err)
		}
		log.Printf("fetched message %d/%d (UID %d)", msgFuture.SeqNum, mailbox.NumMessages, msg.UID)
		header := msg.FindBodySection(bodySection)
		if header == nil {
			log.Print("did not find headers in response, skipping")
			continue
		}
		err = os.WriteFile(tmpfile.Name(), header, 0600)
		if err != nil {
			return fmt.Errorf("failed to write header to temp file %q: %w", tmpfile.Name(), err)
		}

		stdoutBuf.Reset()
		stderrBuf.Reset()
		log.Print("running sieve-test")
		cmd := exec.Command("sieve-test", sieveArgs...)
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		err = cmd.Run()
		if err != nil {
			return fmt.Errorf("sieve-test failed: %s\nstdout:\n%s\nstderr:\n%s", err, &stdoutBuf, &stdoutBuf)
		}
		scanner := bufio.NewScanner(bytes.NewReader(stdoutBuf.Bytes()))
		parseState := expectPerformedActions
		for scanner.Scan() {
			line := scanner.Text()
			switch parseState {
			case expectPerformedActions:
				if line == "" {
					break
				}
				if want := "Performed actions:"; line != want {
					return fmt.Errorf("cannot parse sieve-test output, expected %q, got %q, full output:\n%s", want, line, &stdoutBuf)
				}
				parseState = inPerformedActions
			case inPerformedActions:
				line = strings.TrimLeft(line, " \t")
				if line == "" {
					break
				}
				if line == "(none)" {
					break
				}
				if line == "Implicit keep:" {
					parseState = inImplicitKeep
					break
				}
				destination, ok := strings.CutPrefix(line, "* store message in folder: ")
				if ok {
					if destination == mailboxName {
						log.Printf("fileinto %q (current directory)", destination)
						break
					}
					log.Printf("fileinto %q", destination)
					moves := movesByDestination[destination]
					moves.AddNum(msg.UID)
					movesByDestination[destination] = moves
					break
				}
				return fmt.Errorf("cannot parse performed action %q, full output:\n%s", line, &stdoutBuf)
			case inImplicitKeep:
				line = strings.TrimLeft(line, " \t")
				if line == "" {
					break
				}
				if line == "(none)" {
					break
				}
				if strings.HasPrefix(line, "* store message in folder: ") {
					// It might be useful to be able to define a destination for implicit keeps, but I don't need that at the moment.
					log.Print("Implicit keep. Skipping message because no filters apply.")
					break
				}
				return fmt.Errorf("cannot parse implicit keep %q, full output:\n%s", line, &stdoutBuf)
			}
		}
		if err := scanner.Err(); err != nil {
			// reading from a bytes.Buffer should never fail
			return fmt.Errorf("inexplicable error during scanning of sieve-test output: %w", err)
		}
		if parseState != inImplicitKeep {
			return fmt.Errorf("did not find Implicit keep section in sieve-test output:\n%s", &stdoutBuf)
		}
	}
	var errs []error
	for dst, msgs := range movesByDestination {
		if !execute {
			log.Printf("%4d %s", len(msgs), dst)
		} else {
			_, err := c.Move(msgs, dst).Wait()
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to perform %d _(s) to %q: %w", len(msgs), dst, err))
			} else {
				log.Printf("moved %d messages to %q", len(msgs), dst)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	return nil
}

type ParseState int

const (
	expectPerformedActions ParseState = iota + 1
	inPerformedActions
	inImplicitKeep
)

func allAndClose[Elem any](iterator interface {
	Next() *Elem
	io.Closer
}) iter.Seq2[*Elem, error] {
	return func(yield func(*Elem, error) bool) {
		broke := false
		defer func() {
			err := iterator.Close()
			if err != nil && !broke {
				yield(nil, err)
			}
		}()
		for elem := range all(iterator) {
			if !yield(elem, nil) {
				broke = true
				return
			}
		}
	}
}

func all[Elem any](iterator interface {
	Next() *Elem
}) iter.Seq[*Elem] {
	return func(yield func(*Elem) bool) {
		for {
			elem := iterator.Next()
			if elem == nil {
				return
			}
			if !yield(elem) {
				return
			}
		}
	}
}
