/*
 * tssplit - minimal-CPU MPTS -> SPTS splitter (UDP / RTP multicast in, UDP out)
 *
 * Joins the MPTS multicast group ONCE and emits one SPTS per selected program.
 * Per TS packet the work is: read PID -> one table lookup -> memcpy if wanted.
 * PAT is regenerated per output (single program). PMT, ES and PCR PIDs are
 * passed through untouched. No demuxing of payloads, no decoding.
 *
 * Input may be raw UDP TS or RTP (any payload type, CSRC/extension/padding ok).
 *
 * Build:  gcc -O2 -march=native -Wall -o tssplit tssplit.c
 *
 * Usage:
 *   tssplit -i 239.2.2.2:1234 -s 101=239.3.3.1:1234
 *   tssplit -i 239.2.2.2:1234 -I 10.0.0.5 -S 113.83.32.204 \
 *           -s 101=239.3.3.1:1234 -s 102=239.3.3.2:1234 -O 10.0.0.5 -t 4
 *
 *   -i group:port     input multicast (or unicast bind) address
 *   -I ip             interface IP to join on (default: any)
 *   -S ip             source IP for SSM join (only accept this sender)
 *   -s prog=ip:port   program_number -> output address (repeat, max 32)
 *   -O ip             output interface IP (default: routing table)
 *   -t ttl            output multicast TTL (default 8)
 *   -q                quiet (no periodic stats)
 */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#define TS        188
#define TS_PER_DG 7
#define DG        (TS * TS_PER_DG)
#define RX_BATCH  64
#define RX_SZ     2048
#define TX_Q      512
#define MAX_OUT   32
#define MAX_ES    64

typedef struct {
    uint8_t d[1024 + 8];
    int len, active;
} sec_t;

typedef struct {
    int prog;
    struct sockaddr_in dst;
    int pmt_pid, pmt_ver, pcr_pid, n_es;
    uint16_t es[MAX_ES];
    uint8_t pat_cc, pat_ver;
    sec_t pmt_sec;
    uint8_t buf[DG];
    int nbuf;
    uint64_t pkts;
} out_t;

static out_t outs[MAX_OUT];
static int n_out;
static uint32_t pid_mask[8192];   /* which outputs forward this PID */
static uint32_t pmt_mask[8192];   /* which outputs parse this PID as PMT */
static sec_t pat_sec;
static int tsid = 1;
static int tx_fd;
static volatile sig_atomic_t stop;

static uint8_t rx_buf[RX_BATCH][RX_SZ];
static struct iovec rx_iov[RX_BATCH];
static struct mmsghdr rx_msg[RX_BATCH];
static uint8_t tx_buf[TX_Q][DG];
static struct iovec tx_iov[TX_Q];
static struct mmsghdr tx_msg[TX_Q];
static int tx_n;

/* ---------- MPEG-2 CRC32 ---------- */
static uint32_t crc_tab[256];
static void crc_init(void) {
    for (uint32_t i = 0; i < 256; i++) {
        uint32_t c = i << 24;
        for (int j = 0; j < 8; j++) c = (c & 0x80000000) ? (c << 1) ^ 0x04C11DB7 : c << 1;
        crc_tab[i] = c;
    }
}
static uint32_t crc32_mpeg(const uint8_t *p, int n) {
    uint32_t c = 0xFFFFFFFF;
    while (n--) c = (c << 8) ^ crc_tab[((c >> 24) ^ *p++) & 0xFF];
    return c;
}

/* ---------- output ---------- */
static void tx_flush(void) {
    int sent = 0;
    while (sent < tx_n) {
        int r = sendmmsg(tx_fd, tx_msg + sent, tx_n - sent, 0);
        if (r < 0) {
            if (errno == EINTR) continue;
            static int warned;
            if (!warned++) perror("sendmmsg");
            break;
        }
        sent += r;
    }
    tx_n = 0;
}

static void out_put(out_t *o, const uint8_t *pkt) {
    memcpy(o->buf + o->nbuf, pkt, TS);
    o->nbuf += TS;
    o->pkts++;
    if (o->nbuf < DG) return;
    if (tx_n == TX_Q) tx_flush();
    memcpy(tx_buf[tx_n], o->buf, DG);
    tx_iov[tx_n].iov_base = tx_buf[tx_n];
    tx_iov[tx_n].iov_len = DG;
    memset(&tx_msg[tx_n].msg_hdr, 0, sizeof(struct msghdr));
    tx_msg[tx_n].msg_hdr.msg_name = &o->dst;
    tx_msg[tx_n].msg_hdr.msg_namelen = sizeof(o->dst);
    tx_msg[tx_n].msg_hdr.msg_iov = &tx_iov[tx_n];
    tx_msg[tx_n].msg_hdr.msg_iovlen = 1;
    tx_n++;
    o->nbuf = 0;
}

/* ---------- PSI ---------- */
/* returns 1 when a complete, CRC-valid section sits in s->d (length s->len) */
static int sec_feed(sec_t *s, const uint8_t *p) {
    int afc = (p[3] >> 4) & 3;
    if (!(afc & 1)) return 0;
    int off = 4;
    if (afc & 2) off += 1 + p[4];
    if (off >= TS) return 0;
    const uint8_t *pl = p + off;
    int n = TS - off;
    if (p[1] & 0x40) {
        int ptr = pl[0];
        pl += 1 + ptr;
        n -= 1 + ptr;
        if (n <= 0) return 0;
        s->len = 0;
        s->active = 1;
    } else if (!s->active) {
        return 0;
    }
    int room = (int)sizeof(s->d) - s->len;
    if (n > room) n = room;
    memcpy(s->d + s->len, pl, n);
    s->len += n;
    if (s->len < 3) return 0;
    if (s->d[0] == 0xFF) { s->active = 0; return 0; }
    int tot = 3 + (((s->d[1] & 0x0F) << 8) | s->d[2]);
    if (tot > (int)sizeof(s->d) || tot < 12) { s->active = 0; return 0; }
    if (s->len < tot) return 0;
    s->active = 0;
    s->len = tot;
    return crc32_mpeg(s->d, tot) == 0;
}

static void rebuild_masks(void) {
    memset(pid_mask, 0, sizeof(pid_mask));
    memset(pmt_mask, 0, sizeof(pmt_mask));
    for (int i = 0; i < n_out; i++) {
        out_t *o = &outs[i];
        uint32_t b = 1u << i;
        if (o->pmt_pid < 0) continue;
        pid_mask[o->pmt_pid] |= b;
        pmt_mask[o->pmt_pid] |= b;
        if (o->pcr_pid > 0 && o->pcr_pid < 0x1FFF) pid_mask[o->pcr_pid] |= b;
        for (int k = 0; k < o->n_es; k++) pid_mask[o->es[k]] |= b;
    }
    pid_mask[0] = 0; /* PAT is always regenerated, never passed */
}

static void emit_pat(out_t *o) {
    uint8_t pk[TS];
    memset(pk, 0xFF, TS);
    pk[0] = 0x47; pk[1] = 0x40; pk[2] = 0x00;
    pk[3] = 0x10 | o->pat_cc;
    o->pat_cc = (o->pat_cc + 1) & 0x0F;
    pk[4] = 0x00;                         /* pointer_field */
    uint8_t *s = pk + 5;
    s[0] = 0x00; s[1] = 0xB0; s[2] = 13;  /* table_id, section_length */
    s[3] = tsid >> 8; s[4] = tsid & 0xFF;
    s[5] = 0xC1 | (o->pat_ver << 1);
    s[6] = 0; s[7] = 0;
    s[8] = o->prog >> 8; s[9] = o->prog & 0xFF;
    s[10] = 0xE0 | (o->pmt_pid >> 8); s[11] = o->pmt_pid & 0xFF;
    uint32_t c = crc32_mpeg(s, 12);
    s[12] = c >> 24; s[13] = c >> 16; s[14] = c >> 8; s[15] = c;
    out_put(o, pk);
}

static void handle_pat(const uint8_t *d, int len) {
    if (d[0] != 0x00) return;
    tsid = (d[3] << 8) | d[4];
    int changed = 0;
    for (int i = 0; i < n_out; i++) {
        out_t *o = &outs[i];
        for (int j = 8; j + 4 <= len - 4; j += 4) {
            int prog = (d[j] << 8) | d[j + 1];
            int pid = ((d[j + 2] & 0x1F) << 8) | d[j + 3];
            if (prog != o->prog) continue;
            if (pid != o->pmt_pid) {
                fprintf(stderr, "[prog %d] PMT PID 0x%04x\n", o->prog, pid);
                o->pmt_pid = pid;
                o->pmt_ver = -1;
                o->n_es = 0;
                o->pcr_pid = -1;
                o->pat_ver = (o->pat_ver + 1) & 0x1F;
                o->pmt_sec.active = 0;
                changed = 1;
            }
            break;
        }
    }
    if (changed) rebuild_masks();
    for (int i = 0; i < n_out; i++)
        if (outs[i].pmt_pid >= 0) emit_pat(&outs[i]);
}

static void handle_pmt(out_t *o, const uint8_t *d, int len) {
    if (d[0] != 0x02) return;
    int prog = (d[3] << 8) | d[4];
    if (prog != o->prog) return;            /* shared PMT PID, other program */
    int ver = (d[5] >> 1) & 0x1F;
    if (ver == o->pmt_ver) return;
    o->pmt_ver = ver;
    o->pcr_pid = ((d[8] & 0x1F) << 8) | d[9];
    int i = 12 + (((d[10] & 0x0F) << 8) | d[11]);
    int end = len - 4;
    o->n_es = 0;
    fprintf(stderr, "[prog %d] PMT v%d PCR 0x%04x ES:", o->prog, ver, o->pcr_pid);
    while (i + 5 <= end) {
        int st = d[i];
        int pid = ((d[i + 1] & 0x1F) << 8) | d[i + 2];
        int eil = ((d[i + 3] & 0x0F) << 8) | d[i + 4];
        if (o->n_es < MAX_ES) o->es[o->n_es++] = pid;
        fprintf(stderr, " 0x%04x(type 0x%02x)", pid, st);
        i += 5 + eil;
    }
    fprintf(stderr, "\n");
    rebuild_masks();
}

/* ---------- per-packet path ---------- */
static inline void handle_ts(const uint8_t *p) {
    int pid = ((p[1] & 0x1F) << 8) | p[2];
    if (pid == 0) {
        if (sec_feed(&pat_sec, p)) handle_pat(pat_sec.d, pat_sec.len);
        return;
    }
    uint32_t m = pid_mask[pid];
    if (!m) return;
    uint32_t pm = pmt_mask[pid];
    while (pm) {
        int i = __builtin_ctz(pm);
        pm &= pm - 1;
        if (sec_feed(&outs[i].pmt_sec, p)) handle_pmt(&outs[i], outs[i].pmt_sec.d, outs[i].pmt_sec.len);
    }
    while (m) {
        int i = __builtin_ctz(m);
        m &= m - 1;
        out_put(&outs[i], p);
    }
}

static void handle_dgram(uint8_t *p, int len) {
    /* strip RTP if present (any payload type) */
    if (len >= 12 && p[0] != 0x47 && (p[0] & 0xC0) == 0x80) {
        int h = 12 + 4 * (p[0] & 0x0F);
        if (p[0] & 0x10) {
            if (len < h + 4) return;
            h += 4 + 4 * ((p[h + 2] << 8) | p[h + 3]);
        }
        if (p[0] & 0x20) len -= p[len - 1];
        if (h >= len) return;
        p += h;
        len -= h;
    }
    while (len >= TS) {
        if (p[0] != 0x47) { p++; len--; continue; }  /* resync */
        handle_ts(p);
        p += TS;
        len -= TS;
    }
}

/* ---------- setup ---------- */
static int parse_addr(const char *s, struct sockaddr_in *a) {
    char tmp[128];
    if (!strncmp(s, "udp://", 6)) s += 6;
    else if (!strncmp(s, "rtp://", 6)) s += 6;
    if (*s == '@') s++;
    snprintf(tmp, sizeof(tmp), "%s", s);
    char *c = strrchr(tmp, ':');
    if (!c) return -1;
    *c = 0;
    memset(a, 0, sizeof(*a));
    a->sin_family = AF_INET;
    a->sin_port = htons(atoi(c + 1));
    return inet_pton(AF_INET, tmp, &a->sin_addr) == 1 ? 0 : -1;
}

static void on_sig(int s) { (void)s; stop = 1; }

static void usage(void) {
    fprintf(stderr,
        "usage: tssplit -i group:port [-I iface_ip] [-S source_ip]\n"
        "               -s prog=ip:port [-s prog=ip:port ...]\n"
        "               [-O out_iface_ip] [-t ttl] [-q]\n");
    exit(1);
}

int main(int argc, char **argv) {
    struct sockaddr_in in_addr = {0};
    const char *in_if = NULL, *src = NULL, *out_if = NULL;
    int ttl = 8, quiet = 0, have_in = 0, opt;

    while ((opt = getopt(argc, argv, "i:I:S:s:O:t:q")) != -1) {
        switch (opt) {
        case 'i': if (parse_addr(optarg, &in_addr)) usage(); have_in = 1; break;
        case 'I': in_if = optarg; break;
        case 'S': src = optarg; break;
        case 'O': out_if = optarg; break;
        case 't': ttl = atoi(optarg); break;
        case 'q': quiet = 1; break;
        case 's': {
            if (n_out == MAX_OUT) { fprintf(stderr, "max %d outputs\n", MAX_OUT); exit(1); }
            char *eq = strchr(optarg, '=');
            if (!eq) usage();
            out_t *o = &outs[n_out];
            memset(o, 0, sizeof(*o));
            o->prog = atoi(optarg);
            o->pmt_pid = -1; o->pmt_ver = -1; o->pcr_pid = -1;
            if (parse_addr(eq + 1, &o->dst)) usage();
            n_out++;
            break;
        }
        default: usage();
        }
    }
    if (!have_in || !n_out) usage();
    crc_init();

    /* input socket */
    int rx = socket(AF_INET, SOCK_DGRAM, 0);
    int one = 1, rcvbuf = 32 << 20;
    setsockopt(rx, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    if (setsockopt(rx, SOL_SOCKET, SO_RCVBUFFORCE, &rcvbuf, sizeof(rcvbuf)) < 0)
        setsockopt(rx, SOL_SOCKET, SO_RCVBUF, &rcvbuf, sizeof(rcvbuf));
    if (bind(rx, (struct sockaddr *)&in_addr, sizeof(in_addr)) < 0) { perror("bind"); return 1; }

    if (IN_MULTICAST(ntohl(in_addr.sin_addr.s_addr))) {
        struct in_addr ifa = { .s_addr = htonl(INADDR_ANY) };
        if (in_if) inet_pton(AF_INET, in_if, &ifa);
        if (src) {
            struct ip_mreq_source mr = { .imr_multiaddr = in_addr.sin_addr, .imr_interface = ifa };
            inet_pton(AF_INET, src, &mr.imr_sourceaddr);
            if (setsockopt(rx, IPPROTO_IP, IP_ADD_SOURCE_MEMBERSHIP, &mr, sizeof(mr)) < 0) {
                perror("IP_ADD_SOURCE_MEMBERSHIP"); return 1;
            }
        } else {
            struct ip_mreq mr = { .imr_multiaddr = in_addr.sin_addr, .imr_interface = ifa };
            if (setsockopt(rx, IPPROTO_IP, IP_ADD_MEMBERSHIP, &mr, sizeof(mr)) < 0) {
                perror("IP_ADD_MEMBERSHIP"); return 1;
            }
        }
    }

    /* output socket */
    tx_fd = socket(AF_INET, SOCK_DGRAM, 0);
    int sndbuf = 8 << 20;
    unsigned char t = ttl, loop = 1;
    setsockopt(tx_fd, SOL_SOCKET, SO_SNDBUF, &sndbuf, sizeof(sndbuf));
    setsockopt(tx_fd, IPPROTO_IP, IP_MULTICAST_TTL, &t, sizeof(t));
    setsockopt(tx_fd, IPPROTO_IP, IP_MULTICAST_LOOP, &loop, sizeof(loop));
    if (out_if) {
        struct in_addr oa;
        inet_pton(AF_INET, out_if, &oa);
        setsockopt(tx_fd, IPPROTO_IP, IP_MULTICAST_IF, &oa, sizeof(oa));
    }

    for (int i = 0; i < RX_BATCH; i++) {
        rx_iov[i].iov_base = rx_buf[i];
        rx_iov[i].iov_len = RX_SZ;
        rx_msg[i].msg_hdr.msg_iov = &rx_iov[i];
        rx_msg[i].msg_hdr.msg_iovlen = 1;
    }

    signal(SIGINT, on_sig);
    signal(SIGTERM, on_sig);
    fprintf(stderr, "tssplit: %d output(s), waiting for PAT...\n", n_out);

    struct timespec last, now;
    clock_gettime(CLOCK_MONOTONIC_COARSE, &last);
    uint64_t prev[MAX_OUT] = {0};

    while (!stop) {
        int n = recvmmsg(rx, rx_msg, RX_BATCH, MSG_WAITFORONE, NULL);
        if (n < 0) {
            if (errno == EINTR) continue;
            perror("recvmmsg");
            break;
        }
        for (int i = 0; i < n; i++) handle_dgram(rx_buf[i], rx_msg[i].msg_len);
        if (tx_n) tx_flush();

        if (!quiet) {
            clock_gettime(CLOCK_MONOTONIC_COARSE, &now);
            double dt = (now.tv_sec - last.tv_sec) + (now.tv_nsec - last.tv_nsec) / 1e9;
            if (dt >= 10.0) {
                for (int i = 0; i < n_out; i++) {
                    fprintf(stderr, "[prog %d] %.2f Mbps\n", outs[i].prog,
                            (outs[i].pkts - prev[i]) * TS * 8.0 / dt / 1e6);
                    prev[i] = outs[i].pkts;
                }
                last = now;
            }
        }
    }
    fprintf(stderr, "tssplit: exit\n");
    return 0;
}

