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

/* include/linux/local_lock_internal.h __local_lock_acquire: _Generic((lock), local_trylock_t *: ...,
 * local_lock_t *: (void)0), where the control is this_cpu_ptr()'s typeof expression and the
 * associations name typedefs. The selected association is local_lock_t's, so __tl is never read. */
typedef struct dd_lock {
	int held;
} dd_lock_t;
typedef struct dd_trylock {
	int acquired;
} dd_trylock_t;
dd_lock_t dd_lock_one;

static inline void dd_lock_acquire(dd_lock_t *l)
{
	(void)l;
}

/* this_cpu_ptr(ptr): RELOC_HIDE((typeof(*(ptr)) *)(unsigned long)(ptr), offset), whose value has type
 * typeof(ptr): clang spells the type of the _Generic's control with typeof. */
#define dd_hide(ptr) ({ unsigned long __ptr = (unsigned long)(ptr); (typeof(ptr))__ptr; })
#define dd_this_ptr(ptr) dd_hide((typeof(*(ptr)) *)((unsigned long)(ptr)))

#define dd_local_lock(lock)                                                         \
	do {                                                                        \
		dd_trylock_t *__tl;                                                 \
		dd_lock_t *__l;                                                     \
		__l = (dd_lock_t *)(lock);                                          \
		__tl = (dd_trylock_t *)__l;                                         \
		_Generic((lock), dd_trylock_t *: ({ do { __tl->acquired = 1; } while (0); }), dd_lock_t *: (void)0); \
		dd_lock_acquire(__l);                                               \
	} while (0)

void dd_generic_lock(void)
{
	dd_local_lock(dd_this_ptr(&dd_lock_one));
}

/* mm/mm_init.c free_area_init_core: int nid = pgdat->node_id, then zone_init_internals(zone, j, nid, ...),
 * a static callee that never reads nid without NUMA. The local is read only by an argument the image
 * decides, so its initial value is the image's to decide too. */
static void dd_init_nid(struct box *b, long nid)
{
	b->size = 1;
}

void dd_conditional_local(struct box *b, struct dd_holder *h)
{
	long nid = h->ctx.id;

	dd_init_nid(b, nid);
}

/* mm/percpu.c pcpu_dump_alloc_info: v = num_possible_cpus(); while (v /= 10) cpu_width++;. The value of
 * v /= 2 is the loop's condition, so v is read and its stores are code. (A divisor of 2 needs no
 * constant in .rodata, so the fixture's text addresses stay where the contract graph names them.) */
int dd_digits(struct dd_holder *h)
{
	int width = 1;
	long v;

	v = h->ctx.id;
	while (v /= 2)
		width++;
	return width;
}

/* include/linux/sysfs.h without CONFIG_SYSFS: static inline int sysfs_emit(char *buf, const char *fmt,
 * ...) { return 0; }. Arguments past the named parameters of a function that never reads them are
 * not computed (drivers/base/topology.c cpu_capacity_show). One that reads them with va_arg keeps
 * them. (No string literal: one in .rodata would move the fixture's text.) */
static inline int dd_emit(char *buf, const char *fmt, ...)
{
	return 0;
}

int dd_variadic(char *buf, struct box *b)
{
	return dd_emit(buf, buf, b->size);
}

static inline long dd_emit_used(const char *fmt, ...)
{
	__builtin_va_list ap;
	long v;

	__builtin_va_start(ap, fmt);
	v = __builtin_va_arg(ap, long);
	__builtin_va_end(ap);
	return v;
}

long dd_variadic_used(char *buf, struct box *b)
{
	return dd_emit_used(buf, b->size);
}

/* drivers/base/dd.c __device_attach: if (dev->parent) pm_runtime_get_sync(dev->parent), where
 * pm_runtime_get_sync without CONFIG_PM has no effect. The whole statement is not code. */
static inline int dd_pm_get(struct box *b)
{
	return 1;
}

void dd_if_without_effect(struct box *b)
{
	if (b->first)
		dd_pm_get(b);
}

void dd_if_with_effect(struct box *b)
{
	if (b->first)
		obj_free(b);
}

/* include/linux/fs.h IS_DAX(inode): ((inode)->i_flags & S_DAX), with S_DAX 0 without CONFIG_FS_DAX.
 * Anything and 0 is 0: the flags are never read. */
#define DD_S_DAX 0

int dd_masked(struct box *b)
{
	return (b->size & DD_S_DAX) != 0;
}

/* include/linux/instrumented.h: the WARN_ON_ONCE under IS_ENABLED(CONFIG_DEBUG_ATOMIC) is not
 * compiled, so a helper whose only other work is a read has no effect, and a call of it whose value
 * is unused is not code (fsnotify_open, kswapd_age_node). A helper with a live asm volatile stays. */
#define DD_DEBUG 0

static inline long dd_read_checked(struct box *b)
{
	if (DD_DEBUG)
		__asm__ volatile("ebreak");
	return b->size;
}

void dd_dead_effect(struct box *b)
{
	dd_read_checked(b);
}

static inline long dd_read_fenced(struct box *b)
{
	__asm__ volatile("" ::: "memory");
	return b->size;
}

void dd_live_effect(struct box *b)
{
	dd_read_fenced(b);
}

/* mm/slub.c calculate_sheaf_capacity -> size_mul: __builtin_mul_overflow stores only into the
 * caller's local, so the helper has no effect (bootstrap_kmalloc_sheaves). */
static inline long dd_mul(long a, long b)
{
	long r;

	if (__builtin_mul_overflow(a, b, &r))
		return -1;
	return r;
}

void dd_overflow(struct box *b)
{
	dd_mul(b->size, 2);
}

/* mm/vmscan.c can_demote: if (!pgdat || !numa_demotion_enabled) return false, with
 * numa_demotion_enabled false without NUMA. a || 1 is 1, and a && 0 is 0, whatever a is: only a's
 * effects are code (shrink_node's NODE_DATA read). */
#define dd_demotion_enabled 0

static inline int dd_split_init(struct box *b)
{
	return 1;
}

int dd_absorbed_or(struct box *b)
{
	if (!b->first || !dd_demotion_enabled)
		return 0;
	return 1;
}

int dd_absorbed_and(struct box *b)
{
	return b->size && !dd_split_init(b);
}

/* drivers/base/core.c fwnode_links_check_suppliers: list_for_each_entry(link, &fwnode->suppliers,
 * c_hook) is a for loop a macro writes, with a condition that is not a constant: the compiler may
 * assume it ends, so a call whose value is unused goes (waiting_for_supplier_show without SYSFS). */
static int dd_count_links(struct list_node *head)
{
	struct list_node *pos;

	for (pos = head->next; pos != head; pos = pos->next)
		if (pos->prev == head)
			return 1;
	return 0;
}

/* dd_for_each is in mini.h, as list_for_each_entry is in include/linux/list.h. */

static int dd_count_links_macro(struct list_node *head)
{
	struct list_node *pos;

	dd_for_each(pos, head)
		if (pos->prev == head)
			return 1;
	return 0;
}

void dd_unused_loop_call(struct list_node *head)
{
	int val;

	val = dd_count_links(head) + dd_count_links_macro(head);
	dd_emit((char *)0, (char *)0, val);
}

/* scoped_guard(mutex, &fwnode_link_lock) val = ...: a for loop whose body is an expression
 * statement, a store nothing reads. */
void dd_unused_in_body(struct box *b)
{
	long val;
	int once;

	for (once = 0; once < 1; once++)
		val = b->size;
	if (b->first)
		val = b->size;
	dd_emit((char *)0, (char *)0, val);
}

/* An if with one branch an expression statement without effect, the other with one: the
 * condition is code, the branch without effect is not. */
void dd_if_mixed(struct box *b)
{
	if (b->first)
		dd_read_checked(b);
	else
		obj_free(b);
}

/* include/linux/mm.h pagetable_pte_ctor: mm != &init_mm && !ptlock_init(ptdesc). ptlock_init
 * always gives true but initializes the lock: the left operand decides whether that runs. */
static inline int dd_lock_init(struct box *b)
{
	b->size = 0;
	return 1;
}

int dd_absorbed_effect(struct box *b, struct box *c)
{
	if (c->first && !dd_lock_init(b))
		return 0;
	return 1;
}

/* kmalloc_nolock_noprof without __CMPXCHG_DOUBLE: s = kmalloc_slab(size, NULL, alloc_gfp, _RET_IP_),
 * whose value only a stub takes: _RET_IP_ only computes a value. */
static inline struct item *dd_slab(struct box *b, unsigned long caller)
{
	return b->first;
}

static inline int dd_cache_debug(struct item *i)
{
	return 0;
}

struct item *dd_return_address(struct box *b)
{
	struct item *s = dd_slab(b, (unsigned long)__builtin_return_address(0));

	if (!dd_cache_debug(s))
		return NULL;
	return s;
}

/* rcu_start_exp_gp_kworker without CONFIG_RCU_EXP_KTHREAD: struct sched_param param =
 * { .sched_priority = kthread_prio }, whose address only a call under IS_ENABLED takes. */
void dd_struct_unread(void)
{
	struct box param = { .size = dd_flag };

	if (DD_DEBUG)
		obj_free(&param);
}

/* string_get_size's static const char *const *const units_str[] = { units_10, units_2 }: the
 * compiler lays the table out as data, so its initializer is not code of the function. (Inline and
 * not called, so that the image's other functions and objects keep their places.) */
static inline long *dd_unit(int base, int i)
{
	static long units_10[2];
	static long units_2[2];
	static long *units_str[] = { units_10, units_2 };

	return &units_str[base][i];
}

/* flush_all_rcu_sheaves with CONFIG_SLUB_TINY: list_for_each_entry(s, &slab_caches, list) { if
 * (!cache_has_sheaves(s)) continue; flush_rcu_sheaves_on_cache(s); }. The loop stores only its
 * cursor, which nothing after it reads: it has no effect, and C11 lets the compiler assume it ends. */
static inline int dd_has_sheaves(struct list_node *pos)
{
	return !DD_TINY && pos->next;
}

void dd_loop_without_effect(struct box *b)
{
	struct list_node *pos;

	dd_for_each(pos, &b->hash) {
		if (!dd_has_sheaves(pos))
			continue;
		obj_free(pos);
	}
	obj_free(b);
}

/* The same loop whose cursor is read after it: the loop computes that value. */
struct list_node *dd_loop_cursor_read(struct box *b)
{
	struct list_node *pos;

	dd_for_each(pos, &b->hash)
		if (!dd_has_sheaves(pos))
			continue;
	return pos;
}

/* rcutree_online_cpu: if (rcu_scheduler_active == RCU_SCHEDULER_INACTIVE) return 0; then only
 * tick_dep_clear(), empty without NO_HZ_FULL, and return 0: either way the function gives 0. */
static inline void dd_tick_clear(struct box *b)
{
}

int dd_same_outcome(struct box *b)
{
	if (b->size == 0)
		return 0;
	dd_tick_clear(b);
	return 0;
}

/* clk_pm_runtime_put without CONFIG_PM: if (!core->rpm_enabled) return; pm_runtime_put_sync(core->dev); */
void dd_same_outcome_void(struct box *b)
{
	if (!b->first)
		return;
	dd_tick_clear(b);
}

/* clk_pm_runtime_get without CONFIG_PM: if (!core->rpm_enabled) return 0; return
 * pm_runtime_resume_and_get(core->dev);, a stub that gives 0. */
static inline int dd_resume(struct box *b)
{
	return 0;
}

int dd_same_outcome_call(struct box *b)
{
	if (!b->first)
		return 0;
	return dd_resume(b);
}

/* vmstat_cpu_dead: node_cpus = cpumask_of_node(node); if (!cpumask_empty(node_cpus)) return 0;
 * node_clear_state(node, N_CPU); return 0;: the local goes with the condition. */
int dd_same_outcome_local(struct box *b)
{
	long n = b->size;

	if (n)
		return 0;
	dd_tick_clear(b);
	return 0;
}

/* Outcomes that differ keep the condition. */
int dd_other_outcome(struct box *b)
{
	if (b->size == 0)
		return 0;
	dd_tick_clear(b);
	return 1;
}

/* clk_core_is_enabled without CONFIG_PM: if (core->rpm_enabled) { pm_runtime_get_noresume(dev);
 * if (!pm_runtime_active(dev)) { ret = false; goto done; } }, where pm_runtime_active gives true. */
static inline int dd_active(struct box *b)
{
	return 1;
}

int dd_dead_jump(struct box *b, struct box *c)
{
	int ret;

	if (b->first) {
		dd_tick_clear(b);
		if (!dd_active(b)) {
			ret = 0;
			goto done;
		}
	}
	ret = ext_check(c);
done:
	return ret;
}

/* ipi_mux_send_mask: icpu = this_cpu_ptr(ipi_mux_pcpu) is overwritten at the top of the loop
 * before any read, and nothing reads it after the loop. */
void dd_overwritten(struct box *b, struct box *c)
{
	struct item *i = b->first;
	struct list_node *pos;

	dd_for_each(pos, &c->hash) {
		i = c->first;
		obj_free(i);
	}
}

/* wait_task_inactive: rq = task_rq(p) is read only by task_on_cpu(rq, p), which ignores it, and
 * rq = task_rq_lock(p, &rf) comes before every other read. */
static inline void dd_check_item(struct item *i, struct box *c)
{
	obj_free(c);
}

void dd_redefined(struct box *b, struct box *c)
{
	struct item *i;

	i = b->first;
	dd_check_item(i, c);
	i = c->first;
	obj_free(i);
}

/* A store whose value a later read can still see keeps its read. */
void dd_not_redefined(struct box *b, struct box *c)
{
	struct item *i;

	i = b->first;
	if (c->size)
		i = c->first;
	obj_free(i);
}
