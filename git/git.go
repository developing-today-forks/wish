package git

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/charmbracelet/wish"
	"github.com/gliderlabs/ssh"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// ErrNotAuthed represents unauthorized access.
var ErrNotAuthed = fmt.Errorf("you are not authorized to do this")

// ErrSystemMalfunction represents a general system error returned to clients.
var ErrSystemMalfunction = fmt.Errorf("something went wrong")

// AccessLevel is the level of access allowed to a repo.
type AccessLevel int

const (
	NoAccess AccessLevel = iota
	ReadOnlyAccess
	ReadWriteAccess
	AdminAccess
)

// PushCallback represents a function that will be called after every push if
// passed to MiddlewareWithPushCallback.
type PushCallback func(string, ssh.PublicKey)

// Auth is an interface that allows for custom authorization implementations.
// Prior to git access, AuthRepo will be called with the ssh.Session public key
// key and the repo name. Implementers return the appropriate AccessLevel.
type Auth interface {
	AuthRepo(string, ssh.PublicKey) AccessLevel
}

// Middleware adds Git server functionality to the ssh.Server. Repos are stored
// in the specified repo directory. The provided Auth implementation will be
// checked for access on a per repo basis for a ssh.Session public key.
func Middleware(repoDir string, auth Auth) wish.Middleware {
	return MiddlewareWithPushCallback(repoDir, auth, nil)
}

// MiddlewareWithPushCallback is the same as Middleware but will call the
// provided callback after a successful push with the repo name and ssh.Session
// public key.
func MiddlewareWithPushCallback(repoDir string, auth Auth, cb PushCallback) wish.Middleware {
	return func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			cmd := s.Command()
			if len(cmd) == 2 {
				gc := cmd[0]
				repo := cmd[1]
				pk := s.PublicKey()
				access := auth.AuthRepo(repo, pk)
				switch gc {
				case "git-receive-pack":
					switch access {
					case ReadWriteAccess, AdminAccess:
						err := gitReceivePack(s, gc, repoDir, repo)
						if err != nil {
							log.Printf("git-receive-pack error: %s", err)
							fatalGit(s, ErrSystemMalfunction)
						}
						if cb != nil {
							cb(repo, pk)
						}
					default:
						fatalGit(s, ErrNotAuthed)
					}
				case "git-upload-archive", "git-upload-pack":
					switch access {
					case ReadOnlyAccess, ReadWriteAccess, AdminAccess:
						err := gitUploadPack(s, gc, repoDir, repo)
						if err != nil {
							log.Printf("%s error: %s", gc, err)
							fatalGit(s, ErrSystemMalfunction)
						}
					default:
						fatalGit(s, ErrNotAuthed)
					}
				}
			}
			sh(s)
		}
	}
}

func gitReceivePack(s ssh.Session, gitCmd string, repoDir string, repo string) error {
	rp := fmt.Sprintf("%s%s", repoDir, repo)
	ctx := s.Context()
	err := ensureRepo(ctx, repoDir, repo)
	if err != nil {
		return err
	}
	err = runCmd(s, "./", gitCmd, rp)
	if err != nil {
		return err
	}
	err = runCmd(s, rp, "git", "update-server-info")
	if err != nil {
		return err
	}
	err = ensureDefaultBranch(s, rp)
	if err != nil {
		return err
	}
	return nil
}

func gitUploadPack(s ssh.Session, gitCmd string, repoDir string, repo string) error {
	rp := fmt.Sprintf("%s%s", repoDir, repo)
	if exists, err := fileExists(rp); exists && err == nil {
		err = runCmd(s, "./", gitCmd, rp)
		if err != nil {
			return err
		}
	}
	return nil
}

func parseKeysFromFile(path string) ([]ssh.PublicKey, error) {
	authedKeys := make([]ssh.PublicKey, 0)
	hasAuth, err := fileExists(path)
	if err != nil {
		return nil, err
	}
	if hasAuth {
		f, err := os.Open(path)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		err = addKeys(scanner, &authedKeys)
		if err != nil {
			return nil, err
		}
	}
	return authedKeys, nil
}

func parseKeysFromString(keys string) ([]ssh.PublicKey, error) {
	authedKeys := make([]ssh.PublicKey, 0)
	scanner := bufio.NewScanner(strings.NewReader(keys))
	err := addKeys(scanner, &authedKeys)
	if err != nil {
		return nil, err
	}
	return authedKeys, nil
}

func addKeys(s *bufio.Scanner, keys *[]ssh.PublicKey) error {
	for s.Scan() {
		pt := s.Text()
		if pt == "" {
			continue
		}
		log.Printf("Adding authorized key: %s", pt)
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pt))
		if err != nil {
			return err
		}
		*keys = append(*keys, pk)
	}
	if err := s.Err(); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func fatalGit(s ssh.Session, err error) {
	// hex length includes 4 byte length prefix and ending newline
	msg := err.Error()
	pktLine := fmt.Sprintf("%04x%s\n", len(msg)+5, msg)
	_, _ = s.Write([]byte(pktLine))
	s.Exit(1)
}

func ensureRepo(ctx context.Context, dir string, repo string) error {
	exists, err := fileExists(dir)
	if err != nil {
		return err
	}
	if !exists {
		err = os.MkdirAll(dir, os.ModeDir|os.FileMode(0700))
		if err != nil {
			return err
		}
	}
	rp := fmt.Sprintf("%s%s", dir, repo)
	exists, err = fileExists(rp)
	if err != nil {
		return err
	}
	if !exists {
		c := exec.CommandContext(ctx, "git", "init", "--bare", rp)
		err = c.Run()
		if err != nil {
			return err
		}
	}
	return nil
}

func runCmd(s ssh.Session, dir, name string, args ...string) error {
	usi := exec.CommandContext(s.Context(), name, args...)
	usi.Dir = dir
	usi.Stdout = s
	usi.Stdin = s
	err := usi.Run()
	if err != nil {
		return err
	}
	return nil
}

func ensureDefaultBranch(s ssh.Session, repoPath string) error {
	r, err := git.PlainOpen(repoPath)
	if err != nil {
		return err
	}
	brs, err := r.Branches()
	if err != nil {
		return err
	}
	defer brs.Close()
	fb, err := brs.Next()
	if err != nil {
		return err
	}
	// Rename the default branch to the first branch available
	_, err = r.Head()
	if err == plumbing.ErrReferenceNotFound {
		err = runCmd(s, repoPath, "git", "branch", "-M", fb.Name().Short())
		if err != nil {
			return err
		}
	}
	if err != nil && err != plumbing.ErrReferenceNotFound {
		return err
	}
	return nil
}
