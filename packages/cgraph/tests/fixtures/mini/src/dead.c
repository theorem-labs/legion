#include "mini.h"

/* Code the compiler removes because nothing it computes is used, or because control never reaches
 * it. Each function is a shape the kernel has; the comment names one kernel site. */

struct dd_ns {
	long id;
	long map[4];
};
struct dd_ns dd_init_ns;
struct box dd_boxes[4];
long dd_offsets[4];
long dd_mask;
int dd_flag;

/* mm/slub.c __slab_free with CONFIG_SLUB_TINY: the first if always returns, so the label after the
 * last return is reached only by a goto behind it. */
#define DD_TINY 1

int dd_dead_label(struct box *b)
{
	if (DD_TINY || b->size)
		return 0;
	if (b->first)
		goto empty;
	return 1;
empty:
	obj_free(b);
	return 2;
}

/* A label a live goto reaches is code, also when the goto comes after it. */
int dd_live_label(struct box *b)
{
	int tries = 0;
again:
	if (b->size > tries++)
		goto out;
	if (tries < 3)
		goto again;
	obj_free(b);
out:
	box_weight();
	return tries;
}

/* lockdep_assert_held(__rq_lockp(rq)) without lockdep: do { (void)(l); } while (0), where l calls an
 * inline helper that only computes an address. */
static inline long *dd_lockp(struct box *b)
{
	return &b->size;
}
#define dd_assert_held(l) do { (void)(l); } while (0)

void dd_void_call(struct box *b)
{
	dd_assert_held(dd_lockp(b));
}

/* cpu_to_node(raw_smp_processor_id()) without NUMA: ((void)(cpu), 0), where cpu reads through an
 * inline helper. */
static inline struct box *dd_current(void)
{
	return &dd_boxes[0];
}
#define dd_node(cpu) ((void)(cpu), 0)

int dd_comma_call(void)
{
	return dd_node(dd_current()->size);
}

/* kernel/cred.c prepare_creds: get_user_ns(new->user_ns); as a statement, where get_user_ns without
 * user namespaces returns &init_user_ns. drivers/clk/clk.c: clk_pm_runtime_put(core), a void
 * function that only reads. */
static inline struct dd_ns *dd_get_ns(struct dd_ns *ns)
{
	(void)ns;
	return &dd_init_ns;
}

static inline int dd_put_stub(struct box *b)
{
	(void)b;
	return -1;
}

static void dd_quiet(struct box *b)
{
	if (!b->size)
		return;
	dd_put_stub(b);
}

void dd_statement_calls(struct box *b, struct dd_ns *ns)
{
	dd_get_ns(ns);
	dd_quiet(b);
}

/* lib/radix-tree.c idr_preload: local_lock() stores this_cpu_ptr(lock) in __tl, and without lockdep
 * local_lock_acquire(__tl) never reads it. slab_post_alloc_hook: flags &= gfp_allowed_mask, where
 * flags is then only passed to stubs. */
static inline void dd_acquire(long *l)
{
	(void)l;
}

static inline void dd_use_flags(long f)
{
	(void)f;
}

void dd_dead_store(int cpu, long flags)
{
	long *tl;

	tl = ({ unsigned long ptr; ptr = (unsigned long)&dd_offsets[cpu]; (long *)ptr; });
	dd_acquire(tl);
	flags &= dd_mask;
	dd_use_flags(flags);
}

/* A loop whose controlling expression is not a constant may be assumed to end (C11 6.8.5p6), so the
 * compiler removes one that only computes an unused value. */
static inline long dd_count(struct box *b)
{
	long sum = 0;

	while (b->size > sum)
		sum++;
	return sum;
}

/* A loop with a constant controlling expression may not end, so it stays, and so do a call to an
 * external function and a store. */
static inline void dd_wait(struct box *b)
{
	for (;;)
		if (b->size)
			break;
}

void dd_kept(struct box *b)
{
	(void)obj_alloc(1);
	dd_count(b);
	dd_wait(b);
	dd_flag++;
}

/* kernel/signal.c: from_kuid_munged(current_user_ns(), uid), where from_kuid_munged passes its
 * namespace only to from_kuid, which ignores it; current_user_ns() returns &init_user_ns. */
static inline long dd_from_inner(struct dd_ns *to, long id)
{
	(void)sizeof(to);
	return id;
}

static inline long dd_from(struct dd_ns *to, long id)
{
	long value = dd_from_inner(to, id);

	if (value == -1)
		value = 0;
	return value;
}

static inline struct dd_ns *dd_current_ns(void)
{
	return &dd_init_ns;
}

long dd_transitive(long id)
{
	return dd_from(dd_current_ns(), id);
}

/* kernel/sched/deadline.c update_stats_wait_start_dl: if (!schedstat_enabled()) return; first, so its
 * parameters are read only in code that is not compiled. */
#define dd_stats_enabled() 0

static inline void dd_stats(struct box *b)
{
	if (!dd_stats_enabled())
		return;
	b->size++;
}

void dd_dead_use(int k)
{
	dd_stats(&dd_boxes[k]);
}

/* drivers/base/core.c device_del: device_remove_file(dev, &dev_attr_dev), an external function that
 * only reads, whose attribute reaches only a stub: a call the compiler removes. */
struct dd_ns dd_attr_dev;

static inline void dd_remove_stub(struct dd_ns *a)
{
	(void)a;
}

void dd_remove_file(struct box *b, struct dd_ns *a)
{
	if (b)
		dd_remove_stub(a);
}

void dd_del(struct box *b)
{
	dd_remove_file(b, &dd_attr_dev);
}

/* The same with a callee that has an effect: whether the address is formed is the image's to
 * tell, because only where the compiler inlines the callee does the value go unused. */
void dd_reset_file(struct box *b, struct dd_ns *a)
{
	b->size = 0;
	dd_remove_stub(a);
}

void dd_reset(struct box *b)
{
	dd_reset_file(b, &dd_attr_dev);
}

/* fs/dcache.c: dentry_cache is runtime_const_ptr(__dentry_cache), which names the symbol only in
 * typeof; the address is patched into the instruction, so no access is compiled. */
struct box *dd_cache;
#define dd_runtime_ptr(sym) ({ typeof(sym) __ret; __asm__("" : "=r"(__ret)); __ret; })

void dd_runtime_free(void)
{
	obj_free(dd_runtime_ptr(dd_cache));
}

/* include/linux/rhashtable.h rht_key_get_hash: a field read in a __builtin_constant_p branch is
 * compiled only where the compiler takes that branch. */
long dd_builtin_field(struct box *b, long n)
{
	if (__builtin_constant_p(n))
		return b->size;
	return n;
}

/* include/linux/compiler.h RELOC_HIDE, under this_cpu_ptr(): asm without volatile and with an
 * output computes only that output, so the compiler removes it when nothing reads the output
 * (local_lock's __tl). src/facts.c fct_asm_kept has the asm that stays. */
#define dd_reloc_hide(ptr) ({ unsigned long __ptr; __asm__("" : "=r"(__ptr) : "0"(ptr)); (long *)__ptr; })

void dd_asm_store(int cpu)
{
	long *tl;

	tl = dd_reloc_hide(&dd_offsets[cpu]);
	dd_acquire(tl);
}

/* drivers/char/random.c fast_mix(unsigned long s[4], ...): a parameter of array type is a pointer,
 * so a store through s[i] is an effect, and the call stays. */
static void dd_mix(long s[2], long v)
{
	s[0] += v;
	s[1] ^= s[0];
}

void dd_array_parameter(long v)
{
	dd_mix(dd_offsets, v);
}

/* kernel/sched/cpupri.c: free_cpumask_var(vec->mask), where the stub's parameter has array type
 * (cpumask_var_t without CPUMASK_OFFSTACK); mm/vma.c: is_mergeable_vm_userfaultfd_ctx(vma,
 * vmg->uffd_ctx), a struct the stub takes by value. Neither argument is computed. */
typedef long dd_mask_t[2];
struct dd_holder {
	dd_mask_t mask;
	struct dd_ns ctx;
};

static inline void dd_free_mask(dd_mask_t mask)
{
}

static inline int dd_mergeable(struct dd_ns ctx)
{
	return 1;
}

int dd_typed_parameters(struct dd_holder *h)
{
	dd_free_mask(h->mask);
	return dd_mergeable(h->ctx);
}

/* kernel/sched/core.c __schedule: psi_sched_switch(prev, next, !task_on_rq_queued(prev) ||
 * prev->se.sched_delayed). task_on_rq_queued reads with READ_ONCE, which the compiler keeps, but the
 * value of || is unused, so prev->se.sched_delayed is never read. */
static inline void dd_switch(struct box *b, int sleep)
{
	(void)sleep;
}

static inline int dd_queued(struct box *b)
{
	return *(volatile long *)&b->size == 1;
}

void dd_partial(struct box *b)
{
	dd_switch(b, !dd_queued(b) || b->first);
}

/* kernel/fork.c mm_alloc: mm_init(mm, current, current_user_ns()), where mm_init is static and not
 * inline and never reads its namespace: the image decides, also for an argument that calls. */
static long dd_init(struct box *b, struct dd_ns *ns)
{
	b->size = 1;
	return b->size;
}

long dd_static_callee(struct box *b)
{
	return dd_init(b, dd_current_ns());
}
