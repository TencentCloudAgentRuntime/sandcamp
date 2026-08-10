#define _GNU_SOURCE

#include <errno.h>
#include <grp.h>
#include <stdio.h>
#include <string.h>
#include <sys/types.h>
#include <unistd.h>

extern char **environ;

static int fail(const char *operation) {
    fprintf(stderr, "campd-launcher: %s: %s\n", operation, strerror(errno));
    return 125;
}

int main(int argc, char **argv) {
    static const char target[] = "/mnt/sandcamp/bin/campd.real";

    if (argc < 1 || argv == NULL) {
        fputs("campd-launcher: invalid argv\n", stderr);
        return 125;
    }
    if (getpid() != 1) {
        fputs("campd-launcher: refusing non-PID-1 invocation\n", stderr);
        return 125;
    }
    if (geteuid() != 0) {
        fputs(
            "campd-launcher: setuid bootstrap did not obtain effective UID 0; "
            "check nosuid and no_new_privs\n",
            stderr
        );
        return 125;
    }
    if (setgroups(0, NULL) != 0) {
        return fail("setgroups failed");
    }
    if (setresgid(0, 0, 0) != 0) {
        return fail("setresgid failed");
    }
    if (setresuid(0, 0, 0) != 0) {
        return fail("setresuid failed");
    }

    argv[0] = (char *)target;
    execve(target, argv, environ);
    return fail("execve campd failed");
}
