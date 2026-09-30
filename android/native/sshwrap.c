/* ssh -> dbclient argument translator.
 *
 * "msnw mosh" runs ssh the OpenSSH way:
 *   ssh -o ProxyCommand=<cmd> [extra options] [user@]host -- mosh-server new ...
 * dropbear's dbclient has a different command line, so this wrapper (installed
 * as "ssh" on the app's PATH) maps the options it understands and execs
 * dbclient (found next to this binary, or via $MSNW_DBCLIENT).
 *
 *   -o ProxyCommand=CMD  -> -J CMD
 *   -o StrictHostKeyChecking=no / accept-new -> -y
 *   -i FILE, -p PORT, -l USER -> same
 *   -o UserKnownHostsFile=..., other -o -> ignored (dbclient keeps known hosts in $HOME/.ssh)
 *   -n -tt -t -q -T -N -f -v -4 -6 -> dropped except -t/-T/-N/-f/-v which dbclient shares
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <libgen.h>
#include <limits.h>

static const char *opt_value(const char *o, const char *key) {
	size_t n = strlen(key);
	if (strncasecmp(o, key, n) == 0 && o[n] == '=') return o + n + 1;
	return NULL;
}

int main(int argc, char **argv) {
	char *out[argc * 2 + 8];
	int n = 0;
	char self[PATH_MAX], dbclient[PATH_MAX];
	const char *env = getenv("MSNW_DBCLIENT");
	if (env) {
		snprintf(dbclient, sizeof dbclient, "%s", env);
	} else {
		ssize_t l = readlink("/proc/self/exe", self, sizeof self - 1);
		if (l < 0) { perror("readlink"); return 127; }
		self[l] = 0;
		snprintf(dbclient, sizeof dbclient, "%s/dbclient", dirname(self));
	}
	out[n++] = dbclient;
	int accept_new = 1; /* unknown host keys are accepted (the app has no way to answer a prompt reliably) */
	int i = 1;
	for (; i < argc; i++) {
		const char *a = argv[i];
		if (strcmp(a, "--") == 0) { i++; break; }
		if (strcmp(a, "-o") == 0 && i + 1 < argc) {
			const char *o = argv[++i], *v;
			if ((v = opt_value(o, "ProxyCommand"))) { out[n++] = "-J"; out[n++] = (char *)v; }
			else if ((v = opt_value(o, "StrictHostKeyChecking"))) accept_new = strcasecmp(v, "yes") != 0;
			continue;
		}
		if (strncmp(a, "-o", 2) == 0 && a[2]) { /* -oKey=Val */
			const char *v;
			if ((v = opt_value(a + 2, "ProxyCommand"))) { out[n++] = "-J"; out[n++] = (char *)v; }
			continue;
		}
		if ((strcmp(a, "-i") == 0 || strcmp(a, "-p") == 0 || strcmp(a, "-l") == 0) && i + 1 < argc) {
			out[n++] = (char *)a; out[n++] = argv[++i]; continue;
		}
		if (strcmp(a, "-t") == 0 || strcmp(a, "-T") == 0 || strcmp(a, "-N") == 0 || strcmp(a, "-f") == 0 || strcmp(a, "-v") == 0) {
			out[n++] = (char *)a; continue;
		}
		if (strcmp(a, "-tt") == 0) { out[n++] = "-t"; continue; }
		if (a[0] == '-' && a[1]) continue; /* other OpenSSH-only flags: -n -q -4 -6 -F ... */
		break; /* destination */
	}
	if (accept_new) out[n++] = "-y";
	if (i < argc) out[n++] = argv[i++];                 /* [user@]host */
	if (i < argc && strcmp(argv[i], "--") == 0) i++;    /* dbclient has no "--"; the rest is the command */
	for (; i < argc; i++) out[n++] = argv[i];
	out[n] = NULL;
	execv(dbclient, out);
	perror(dbclient);
	return 127;
}
