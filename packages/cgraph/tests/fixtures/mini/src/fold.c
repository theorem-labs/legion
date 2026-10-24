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