//go:build ignore

#include "vmlinux.h"
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct notsent_key {
	__u64 cgroup_id;
	__u64 sock_cookie;
	__u32 pid;
	__u32 padding;
	char comm[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} notsent_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} notsent_bytes_max SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} inflight_or_unacked_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} inflight_or_unacked_bytes_max SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} outstanding_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} outstanding_bytes_max SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} snd_wnd_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} snd_wnd_bytes_max SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} snd_cwnd_packets SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} snd_cwnd_packets_max SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} mss_cache_bytes SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} packets_out SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 8192);
	__type(key, struct notsent_key);
	__type(value, __u64);
} total_retrans SEC(".maps");

SEC("fexit/tcp_sendmsg")
int BPF_PROG(trace_tcp_sendmsg, struct sock *sk, struct msghdr *msg, size_t size, int ret)
{
	struct tcp_sock *tp = (struct tcp_sock *)sk;
	struct notsent_key key = {};
	__u64 *prev_max;
	__u32 write_seq = 0;
	__u32 snd_nxt = 0;
	__u32 snd_una = 0;
	__u32 snd_wnd = 0;
	__u32 snd_cwnd = 0;
	__u32 mss_cache_value = 0;
	__u32 packets_out_value = 0;
	__u32 total_retrans_value = 0;
	__u32 notsent = 0;
	__u32 inflight_or_unacked = 0;
	__u32 outstanding = 0;
	__u64 value = 0;
	__u64 pid_tgid = bpf_get_current_pid_tgid();

	(void)msg;
	(void)size;
	(void)ret;

	key.cgroup_id = bpf_get_current_cgroup_id();
	key.sock_cookie = bpf_get_socket_cookie(sk);
	key.pid = pid_tgid >> 32;
	bpf_get_current_comm(&key.comm, sizeof(key.comm));

	write_seq = BPF_CORE_READ(tp, write_seq);
	snd_nxt = BPF_CORE_READ(tp, snd_nxt);
	snd_una = BPF_CORE_READ(tp, snd_una);
	snd_wnd = BPF_CORE_READ(tp, snd_wnd);
	snd_cwnd = BPF_CORE_READ(tp, snd_cwnd);
	mss_cache_value = BPF_CORE_READ(tp, mss_cache);
	packets_out_value = BPF_CORE_READ(tp, packets_out);
	total_retrans_value = BPF_CORE_READ(tp, total_retrans);
	notsent = (__u32)(write_seq - snd_nxt);
	inflight_or_unacked = (__u32)(snd_nxt - snd_una);
	outstanding = (__u32)(write_seq - snd_una);

	value = notsent;
	bpf_map_update_elem(&notsent_bytes, &key, &value, BPF_ANY);
	prev_max = bpf_map_lookup_elem(&notsent_bytes_max, &key);
	if (prev_max == NULL || value > *prev_max)
		bpf_map_update_elem(&notsent_bytes_max, &key, &value, BPF_ANY);

	value = inflight_or_unacked;
	bpf_map_update_elem(&inflight_or_unacked_bytes, &key, &value, BPF_ANY);
	prev_max = bpf_map_lookup_elem(&inflight_or_unacked_bytes_max, &key);
	if (prev_max == NULL || value > *prev_max)
		bpf_map_update_elem(&inflight_or_unacked_bytes_max, &key, &value, BPF_ANY);

	value = outstanding;
	bpf_map_update_elem(&outstanding_bytes, &key, &value, BPF_ANY);
	prev_max = bpf_map_lookup_elem(&outstanding_bytes_max, &key);
	if (prev_max == NULL || value > *prev_max)
		bpf_map_update_elem(&outstanding_bytes_max, &key, &value, BPF_ANY);

	value = snd_wnd;
	bpf_map_update_elem(&snd_wnd_bytes, &key, &value, BPF_ANY);
	prev_max = bpf_map_lookup_elem(&snd_wnd_bytes_max, &key);
	if (prev_max == NULL || value > *prev_max)
		bpf_map_update_elem(&snd_wnd_bytes_max, &key, &value, BPF_ANY);

	value = snd_cwnd;
	bpf_map_update_elem(&snd_cwnd_packets, &key, &value, BPF_ANY);
	prev_max = bpf_map_lookup_elem(&snd_cwnd_packets_max, &key);
	if (prev_max == NULL || value > *prev_max)
		bpf_map_update_elem(&snd_cwnd_packets_max, &key, &value, BPF_ANY);

	value = mss_cache_value;
	bpf_map_update_elem(&mss_cache_bytes, &key, &value, BPF_ANY);

	value = packets_out_value;
	bpf_map_update_elem(&packets_out, &key, &value, BPF_ANY);

	value = total_retrans_value;
	bpf_map_update_elem(&total_retrans, &key, &value, BPF_ANY);
	return 0;
}
