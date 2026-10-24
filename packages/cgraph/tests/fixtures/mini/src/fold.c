#include "mini.h"

/* Code whose facts depend on what the code around it does with it: mostly helpers the image inlines,
 * as link folds them into their callers, where what a caller does with a call decides which of the
 * helper's events are code there. Each function is a shape the kernel has; the comment names one
 * kernel site. The image links this file last, after the stubs, so the code of the files before it
 * keeps its addresses. It has no constant data, which would come before them, and no global, as the
 * image's .bss ends with the stubs' heap, which the image tests read. */

/* include/linux/container_of.h, as src/facts.c gives it. */
#define container_of(ptr, type, member) ({ \
	void *__mptr = (void *)(ptr); \
	_Static_assert(__builtin_types_compatible_p(typeof(*(ptr)), typeof(((type *)0)->member)), \
		       "pointer type mismatch in container_of()"); \
	((type *)(__mptr - __builtin_offsetof(type, member))); })

struct fd_in {
	long a[4];
};
struct fd_out {
	long pad[4];
	struct fd_in m;
	long *ns;
	struct list_node link;
};

/* drivers/base/component.c __aggregate_find: list_for_each_entry(m, &aggregate_devices, node) if
 * (m->parent == parent && ...) return m; return NULL;. The loop stores only its cursor, but it may
 * leave the function with it: the loop is code. */
struct fd_out *fd_find(struct list_node *head, long *ns)
{
	struct list_node *pos;

	dd_for_each(pos, head)
		if (container_of(pos, struct fd_out, link)->ns == ns)
			return container_of(pos, struct fd_out, link);
	return NULL;
}

/* kernel/sys.c validate_prctl_map_addr: for (i = 0; i < ARRAY_SIZE(offsets); i++) { ... if (val >=
 * mmap_max_addr || val < mmap_min_addr) goto out; }: a loop that may jump out is code. */
struct fd_limits {
	long low;
	long v[4];
};

int fd_check_all(struct fd_limits *l)
{
	int i;

	for (i = 0; i < 4; i++)
		if (l->v[i] < l->low)
			goto out;
	return 0;
out:
	obj_free(l);
	return -1;
}

/* mm/swap.c folios_put_refs without THP: folio_order(folio) <= 1 || !folio_test_large_rmappable(folio),
 * where the right operand is 1. folio_order's volatile flags read stays; its read of the order,
 * which only makes the unused value, does not. */
struct fd_folio {
	unsigned long flags;
	unsigned long order;
};

static inline int fd_folio_head(const struct fd_folio *f)
{
	return *(const volatile unsigned long *)&f->flags & 1;
}

static inline unsigned long fd_folio_order(const struct fd_folio *f)
{
	if (!fd_folio_head(f))
		return 0;
	return f->order & 0xff;
}

void fd_unused_order(struct fd_folio *f)
{
	fd_folio_order(f);
}

/* Where the value is used, the read that makes it stays. */
unsigned long fd_used_order(struct fd_folio *f)
{
	return fd_folio_order(f);
}

/* kernel/sched/core.c sched_tick without SCHED_HW_PRESSURE: update_hw_load_avg(rq_clock_task(rq),
 * rq, hw_pressure), a stub that ignores its arguments. rq_clock_task's WARN_ON_ONCE test of
 * rq->clock_update_flags stays; its read of rq->clock_task does not. */
struct fd_rq {
	int flags;
	long clock;
};

static inline long fd_rq_clock(struct fd_rq *rq)
{
	if (rq->flags < 2)
		__asm__ volatile("ebreak");
	return rq->clock;
}

static inline void fd_hw_load(long now, struct fd_rq *rq)
{
}

void fd_unused_clock(struct fd_rq *rq)
{
	fd_hw_load(fd_rq_clock(rq), rq);
}

/* kernel/sched/deadline.c enqueue_task_dl: update_stats_wait_start_dl(dl_rq_of_se(&p->dl), &p->dl),
 * a stub without SCHEDSTATS. dl_rq_of_se returns &rq_of_dl_se(dl_se)->dl, whose rq = dl_se->rq only
 * makes the value; dl_server(dl_se) decides whether task_cpu's READ_ONCE runs. */
struct fd_dl_rq {
	long n;
};
struct fd_runq {
	long pad[2];
	struct fd_dl_rq dl;
};
struct fd_dl_entity {
	int server;
	int cpu;
	struct fd_runq *rq;
	struct fd_runq *rqs[4];
};

static inline struct fd_runq *fd_rq_of_se(struct fd_dl_entity *se)
{
	struct fd_runq *rq = se->rq;

	if (!se->server)
		rq = se->rqs[*(volatile int *)&se->cpu & 3];
	return rq;
}

static inline struct fd_dl_rq *fd_dl_rq_of_se(struct fd_dl_entity *se)
{
	return &fd_rq_of_se(se)->dl;
}

static inline void fd_wait_start(struct fd_dl_rq *dl_rq, struct fd_dl_entity *se)
{
}

void fd_unused_entity(struct fd_dl_entity *se)
{
	fd_wait_start(fd_dl_rq_of_se(se), se);
}

/* fs/namespace.c may_use_mount: is_mounted(&mnt->mnt), where is_mounted takes the mount back with
 * real_mount's container_of, so the machine code never forms &mnt->mnt. */
static inline struct fd_out *fd_real(struct fd_in *p)
{
	return container_of(p, struct fd_out, m);
}

static inline int fd_mounted(struct fd_in *p)
{
	return fd_real(p)->ns != 0;
}

int fd_through_helper(struct fd_out *o)
{
	return fd_mounted(&o->m);
}

/* mark_mounts_for_expiry: the mount is itself a container_of of a list node. */
int fd_from_list(struct list_node *n)
{
	struct fd_out *o = container_of(n, struct fd_out, link);

	return fd_mounted(&o->m);
}

/* kernel/sched/fair.c update_curr_fair: update_curr(cfs_rq_of(&rq->donor->se)), where cfs_rq_of's
 * p = task_of(se) takes the task back and reads its cpu. */
struct fd_task {
	long pad[3];
	int cpu;
	struct fd_in se;
};
struct fd_cpu_rq {
	long pad;
	struct fd_task *donor;
};

static inline int fd_cpu_of(struct fd_in *se)
{
	const struct fd_task *p = container_of(se, struct fd_task, se);

	return p->cpu;
}

int fd_donor_cpu(struct fd_cpu_rq *rq)
{
	return fd_cpu_of(&rq->donor->se);
}

/* A helper that reads through the member's address: the machine code forms it, or adds to it. */
static inline long fd_first(struct fd_in *p)
{
	return p->a[0];
}

long fd_member_read(struct fd_out *o)
{
	return fd_first(&o->m);
}

/* kernel/sched/deadline.c start_dl_timer: dl_rq = dl_rq_of_se(dl_se); rq = rq_of_dl_rq(dl_rq), a
 * container_of that takes back the member dl_rq_of_se's &rq->dl gives. */
struct fd_ent {
	long x;
	struct fd_out *owner;
};

static inline struct fd_in *fd_in_of(struct fd_ent *e)
{
	return &e->owner->m;
}

long *fd_returned(struct fd_ent *e)
{
	struct fd_in *in = fd_in_of(e);
	struct fd_out *o = fd_real(in);

	return o->ns;
}

/* The same member address read through: the machine code adds to it. */
long fd_returned_read(struct fd_ent *e)
{
	return fd_in_of(e)->a[1];
}

/* A container_of of another member does not take the address back: &t->b stays (ld 0x28 is inside
 * it, at b.a[1]). */
struct fd_two {
	struct fd_in a;
	struct fd_in b;
};

static inline long fd_two_read(struct fd_in *p)
{
	return container_of(p, struct fd_two, a)->a.a[1];
}

long fd_not_taken_back(struct fd_two *t)
{
	return fd_two_read(&t->b);
}

/* kernel/signal.c kernel_sigaction: current->sighand->action[sig - 1].sa.sa_handler = action, then
 * sigaddset(&mask, sig) shifts by sig - 1: the register keeps sig - 1, so the store adds the array's
 * offset, 32 + 8 for f, not 32 - 24 + 8. Alone, the store to h folds the constant: 8. */
struct fd_elem {
	long h;
	long f;
	long m;
};
struct fd_table {
	long lock;
	long pad[3];
	struct fd_elem act[8];
};

void fd_index_kept(struct fd_table *t, int k, long v, unsigned long *mask)
{
	t->act[k - 1].f = v;
	*mask = 1UL << (k - 1);
}

void fd_index_folded(struct fd_table *t, int k, long v)
{
	t->act[k - 1].h = v;
}

/* mm/vmscan.c lruvec_lru_size: for_each_managed_zone_pgdat(zone, lruvec_pgdat(lruvec), ...) starts
 * zone at pgdat->node_zones, pgdat a container_of of the lruvec, and steps it: on the first pass the
 * code reaches managed at the list node less 3712 plus 136, then adds a zone's size. */
struct fd_zone {
	long pad[17];
	long managed;
	long rest[134];
};
struct fd_node {
	struct fd_zone zones[3];
	long pad[8];
	struct list_node vec;
};

static inline long fd_managed(const struct fd_zone *z)
{
	return z->managed;
}

long fd_zone_sum(struct list_node *vec, int high)
{
	struct fd_node *node = container_of(vec, struct fd_node, vec);
	struct fd_zone *zone;
	long sum = 0;
	int i;

	for (i = 0, zone = node->zones; i <= high; i++, zone++)
		sum += fd_managed(zone);
	return sum;
}

/* fs/read_write.c pos_from_hilo(high, low): (((loff_t)high << HALF_LONG_BITS) << HALF_LONG_BITS) |
 * low on rv64, where high is shifted out: __riscv_sys_preadv never loads pos_h. */
struct fd_regs {
	unsigned long a3;
	unsigned long a4;
};

static inline unsigned long long fd_hilo(unsigned long high, unsigned long low)
{
	return (((unsigned long long)high << 32) << 32) | low;
}

unsigned long long fd_position(const struct fd_regs *r)
{
	return fd_hilo(r->a4, r->a3);
}

/* kernel/sched/fair.c detach_tasks without SCHEDSTATS: next: if (p->sched_task_hot)
 * schedstat_inc(...), an if under a label whose branch is empty: only its condition's effects are
 * code, and it has none (llvm-objdump: beqz a1, +4; ret; sd zero, 0x8(a0); ret). */
struct fd_task_hot {
	unsigned int hot : 1;
	long n;
};

void fd_label(struct fd_task_hot *p, int k)
{
	if (k)
		goto next;
	p->n = 0;
next:
	if (p->hot)
		do { } while (0);
}

/* mm/readahead.c page_cache_ra_order without THP: start = readahead_index(ractl); index = start;
 * then goto fallback before index changes, and ra->size - (index - start): index - start is 0, so
 * neither is read (llvm-objdump: mv a0, a1; ret). */
struct fd_ra {
	unsigned long idx;
	unsigned long size;
};

static inline int fd_large(void)
{
	return 0;
}

unsigned long fd_copy(struct fd_ra *r, unsigned long n)
{
	unsigned long start = r->idx;
	unsigned long index = start;

	if (!fd_large())
		goto fallback;
	index = r->size;
	index += 2;
fallback:
	return n - (index - start);
}

/* mm/page_alloc.c set_pageblock_migratetype: if (page_group_by_mobility_disabled && migratetype <
 * MIGRATE_PCPTYPES) migratetype = MIGRATE_UNMOVABLE;, which reserve_highatomic_pageblock reaches
 * with MIGRATE_HIGHATOMIC = MIGRATE_PCPTYPES: the && is 0 without its left operand. */
struct fd_block {
	int disabled;
	int type;
};

static inline void fd_set_type(struct fd_block *b, int type)
{
	if (b->disabled && type < 3)
		type = 0;
	b->type = type;
}

void fd_set_high(struct fd_block *b)
{
	fd_set_type(b, 3);
}

void fd_set_low(struct fd_block *b)
{
	fd_set_type(b, 1);
}

/* mm/vmstat.c vmstat_cpu_dead: node_cpus = cpumask_of_node(node); if (!cpumask_empty(node_cpus))
 * return 0; node_clear_state(node, N_CPU); return 0;, where bitmap_empty's effect (find_first_bit)
 * is only beside its __builtin_constant_p path: the image decides, and drops it all (llvm-objdump:
 * li a0, 0x0; ret). */
struct fd_cpus {
	unsigned long bits[1];
};

static inline int fd_empty(const unsigned long *bits, unsigned int n)
{
	if (__builtin_constant_p(n) && n < 64)
		return !(*bits & ((1UL << n) - 1));
	__asm__ volatile("" ::: "memory");
	return !bits[0];
}

static inline void fd_clear_state(int node)
{
}

int fd_cpu_dead(struct fd_cpus *online, int node)
{
	const unsigned long *mask = online->bits;

	if (!fd_empty(mask, 32))
		return 0;
	fd_clear_state(node);
	return 0;
}

/* mm/rmap.c page_vma_mkclean_one without MMU_NOTIFIER: mmu_notifier_range_init(&range, ..., address,
 * vma_address_end(pvmw)) only stores into range, which nothing reads, so vma_address_end's reads are
 * not computed (llvm-objdump: ld a0, 0x0(a0); tail obj_free). */
struct fd_range {
	unsigned long start;
	unsigned long end;
};
struct fd_pvmw {
	unsigned long address;
	unsigned long nr;
};

static inline void fd_range_init(struct fd_range *r, unsigned long start, unsigned long end)
{
	r->start = start;
	r->end = end;
}

static inline void fd_range_start(struct fd_range *r)
{
}

static inline unsigned long fd_address_end(struct fd_pvmw *w)
{
	return w->address + (w->nr << 12);
}

void fd_mkclean(struct fd_pvmw *w)
{
	struct fd_range range;
	unsigned long address = w->address;

	fd_range_init(&range, address, fd_address_end(w));
	fd_range_start(&range);
	obj_free((void *)address);
}

/* The same range read afterwards: the stores and their values are code. */
unsigned long fd_mkclean_read(struct fd_pvmw *w)
{
	struct fd_range range;

	fd_range_init(&range, w->address, fd_address_end(w));
	return range.end;
}
