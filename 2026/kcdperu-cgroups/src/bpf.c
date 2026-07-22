/*
 * eBPF tracer for the demo.
 *
 * Logs openat and openat2 paths issued by Java processes so you can see cgroup
 * path lookups and fallbacks (v1 paths failing vs v2 paths succeeding).
 *
 * Correlates enter and exit events to print path, pid, cgroup id, and return
 * code, giving visibility into which cgroup the JVM is probing.
 *
 * Emits OOM victim events with pid and cgroup id to show when the kernel kills
 * the legacy JVM container instead of letting the app throw OOME.
 */
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

struct open_evt { char path[256]; };

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __uint(max_entries, 1024);
  __type(key, __u32);
  __type(value, struct open_evt);
} inflight SEC(".maps");

SEC("tracepoint/syscalls/sys_enter_openat")
int on_open_enter(struct trace_event_raw_sys_enter *ctx)
{
  char comm[16] = {};
  bpf_get_current_comm(comm, sizeof(comm));
  if (comm[0] != 'j' || comm[1] != 'a' || comm[2] != 'v' || comm[3] != 'a')
    return 0;

  struct open_evt ev = {};

  const char *path = (const char *)ctx->args[1];
  if (bpf_probe_read_user_str(ev.path, sizeof(ev.path), path) <= 0)
    return 0;

  __u32 tid = (__u32)bpf_get_current_pid_tgid();
  bpf_map_update_elem(&inflight, &tid, &ev, BPF_ANY);
  return 0;
}

SEC("tracepoint/syscalls/sys_enter_openat2")
int on_open_enter2(struct trace_event_raw_sys_enter *ctx)
{
  return on_open_enter(ctx);
}

SEC("tracepoint/syscalls/sys_exit_openat")
int on_open_exit(struct trace_event_raw_sys_exit *ctx)
{
  __u32 tid = (__u32)bpf_get_current_pid_tgid();
  struct open_evt *ev = bpf_map_lookup_elem(&inflight, &tid);
  if (!ev)
    return 0;

  __u32 pid = (__u32)(bpf_get_current_pid_tgid() >> 32);
  __u64 cgid = bpf_get_current_cgroup_id();
  bpf_printk("openat pid=%u cgid=%llu ret=%ld %s\n", pid, cgid, (long)ctx->ret, ev->path);
  bpf_map_delete_elem(&inflight, &tid);
  return 0;
}

SEC("tracepoint/syscalls/sys_exit_openat2")
int on_open_exit2(struct trace_event_raw_sys_exit *ctx)
{
  return on_open_exit(ctx);
}

SEC("tracepoint/oom/mark_victim")
int on_oom_victim(struct trace_event_raw_mark_victim *ctx)
{
  /* Kernel 6.8 mark_victim tracepoint only exposes pid; print what is available. */
  __u64 cgid = bpf_get_current_cgroup_id();
  bpf_printk("OOM victim pid=%d cgid=%llu\n", ctx->pid, cgid);
  return 0;
}
