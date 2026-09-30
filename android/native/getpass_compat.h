/* bionic has no getpass(); dropbear's client uses it for password prompts.
 * Included into every dropbear translation unit with -include. It must not
 * pull in <string.h>: autoconf probes rely on strchr being undeclared. */
#ifndef MSNW_GETPASS_COMPAT_H
#define MSNW_GETPASS_COMPAT_H
#include <termios.h>
#include <unistd.h>
#include <fcntl.h>
static inline char *getpass(const char *prompt) {
	static char buf[256];
	int fd = open("/dev/tty", O_RDWR);
	if (fd < 0) fd = 0;
	struct termios t, saved;
	int tty = tcgetattr(fd, &t) == 0;
	if (tty) { saved = t; t.c_lflag &= ~(ECHO); tcsetattr(fd, TCSAFLUSH, &t); }
	if (prompt) { int n = 0; while (prompt[n]) n++; (void)!write(fd, prompt, n); }
	ssize_t n = read(fd, buf, sizeof buf - 1);
	if (n < 0) n = 0;
	buf[n] = 0;
	for (ssize_t i = 0; i < n; i++) if (buf[i] == '\n' || buf[i] == '\r') { buf[i] = 0; break; }
	if (tty) { tcsetattr(fd, TCSAFLUSH, &saved); (void)!write(fd, "\n", 1); }
	if (fd != 0) close(fd);
	return buf;
}
#endif
