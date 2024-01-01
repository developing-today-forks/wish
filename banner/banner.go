package banner

import (
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
)

// Middleware prints a banner at the end of the session.
func Middleware(banner string) wish.Middleware {
	return func(sh ssh.Handler) ssh.Handler {
		return func(s ssh.Session) {
			wish.Println(s, banner)
			sh(s)
		}
	}
}
