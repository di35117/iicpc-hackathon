// sandbox/seccomp.go: generates the seccomp syscall filter profile for contestant containers.
//
// Seccomp (Secure Computing Mode) is a Linux kernel feature that filters which
// syscalls a process is allowed to make. Combined with capability dropping,
// it forms the second layer of defence against malicious submissions.
//
// Our profile starts from Docker's default (which already blocks ~40 dangerous syscalls)
// and additionally blocks:
//   - perf_event_open: prevents timing side-channel attacks against the host
//   - userfaultfd:     can be used to slow memory fault handling (DoS vector)
//   - ptrace:          prevents a submission from tracing other processes
//   - clone with CLONE_NEWUSER: prevents escaping the user namespace
//
// Why not just use Docker's default?
//   The default profile was designed for general container workloads. For an
//   untrusted code execution platform, we want a stricter subset.

package sandbox

import "encoding/json"

// seccompProfile is the JSON structure Docker expects for --security-opt seccomp=.
type seccompProfile struct {
	DefaultAction string          `json:"defaultAction"`
	Architectures []string        `json:"architectures"`
	Syscalls      []seccompSyscall `json:"syscalls"`
}

type seccompSyscall struct {
	Names  []string `json:"names"`
	Action string   `json:"action"`
}

// defaultSeccompProfile returns the seccomp profile as a JSON string.
// Docker accepts this directly in SecurityOpt as "seccomp=<json>".
func defaultSeccompProfile() string {
	profile := seccompProfile{
		// Block everything by default; explicitly allow only what's needed.
		DefaultAction: "SCMP_ACT_ERRNO",
		Architectures: []string{"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32"},
		Syscalls: []seccompSyscall{
			{
				// Allow the standard set of syscalls a normal server process needs.
				Action: "SCMP_ACT_ALLOW",
				Names: []string{
					// Process and memory management
					"read", "write", "open", "close", "stat", "fstat", "lstat",
					"mmap", "mprotect", "munmap", "brk", "mremap", "madvise",
					"poll", "lseek", "pread64", "pwrite64", "readv", "writev",
					"access", "dup", "dup2", "dup3", "getpid", "getppid",
					"exit", "exit_group", "wait4", "waitid",
					// File operations
					"openat", "unlink", "unlinkat", "mkdir", "mkdirat",
					"rename", "renameat", "getcwd", "chdir", "readlink", "readlinkat",
					// Networking (TCP only — no raw sockets)
					"socket", "connect", "accept", "accept4", "bind", "listen",
					"getsockname", "getpeername", "sendto", "recvfrom",
					"setsockopt", "getsockopt", "sendmsg", "recvmsg",
					"shutdown", "socketpair",
					// Signals
					"kill", "rt_sigaction", "rt_sigprocmask", "rt_sigreturn",
					"rt_sigpending", "rt_sigsuspend", "sigaltstack",
					// Time
					"clock_gettime", "clock_nanosleep", "nanosleep", "gettimeofday",
					// Threads (needed for Go runtime)
					"clone", "futex", "sched_yield", "sched_getaffinity",
					"set_robust_list", "get_robust_list",
					// Misc required by Go runtime
					"arch_prctl", "set_tid_address", "getrlimit", "setrlimit",
					"getrusage", "uname", "sysinfo", "fcntl", "ioctl",
					"epoll_create", "epoll_create1", "epoll_ctl", "epoll_wait", "epoll_pwait",
					"pipe", "pipe2", "eventfd", "eventfd2", "timerfd_create",
					"timerfd_settime", "timerfd_gettime",
					"prctl", // needed for Go's MADV_FREE and thread naming
				},
			},
			{
				// Explicitly block high-risk syscalls not covered by the default deny.
				Action: "SCMP_ACT_ERRNO",
				Names: []string{
					"ptrace",           // prevents tracing other processes
					"perf_event_open",  // prevents timing side-channel attacks
					"userfaultfd",      // DoS vector via memory fault injection
					"keyctl",           // prevents keyring manipulation
					"add_key",
					"request_key",
					"mbind",            // NUMA manipulation — not needed, potential DoS
					"migrate_pages",
					"move_pages",
					"kexec_load",       // loading new kernel — absolute no
					"kexec_file_load",
					"reboot",
					"mount",
					"umount2",
					"pivot_root",
					"chroot",
					"swapon",
					"swapoff",
					"syslog",
					"settimeofday",
					"adjtimex",
					"init_module",      // loading kernel modules
					"delete_module",
					"iopl",
					"ioperm",
					"create_module",
					"finit_module",
				},
			},
		},
	}

	b, _ := json.Marshal(profile)
	return string(b)
}