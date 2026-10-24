#include "mini.h"

/* Field offsets as the machine code adds them, and code the compiler never emits. Each function is a
 * shape the kernel has; the comment names one kernel site. */

struct fct_inner {
	long pad;
	int a;
	int b;
};
struct fct_outer {
	long x;
	struct fct_inner in;
	union {
		unsigned long both;
		/* A tagged member without a name (-fms-extensions), as pipe_inode_info's pipe_index. */
		struct fct_index {
			unsigned int head;
			unsigned int tail;
		};
	};
	long stats[8];
	struct fct_inner slots[4];
	struct list_node link;
};

struct fct_outer fct_global;

/* irq_release_resources: struct irq_data *d = &desc->irq_data; d->chip. */
int fct_derived_local(struct fct_outer *o)
{
	struct fct_inner *d = &o->in;

	return d->b;
}

/* current->thread.fstate: one access at the sum of the levels. */
int fct_chain(struct fct_outer *o)
{
	return o->in.b;
}

/* pipe->tail, where tail sits in an unnamed struct pipe_index member. */
unsigned int fct_tagged_anonymous(struct fct_outer *o)
{
	return o->tail;
}

/* pwq->stats[PWQ_STAT_RESCUED]++. */
void fct_constant_element(struct fct_outer *o)
{
	o->stats[3]++;
}

/* s->per_node[node].node. */
int fct_indexed_element(struct fct_outer *o, int k)
{
	return o->slots[k].b;
}

/* hlist_for_each_entry's (&core->children)->first. */
int fct_address_base(struct fct_outer *o)
{
	return (&o->in)->a;
}

/* irq_desc_get_irq_data(desc)->chip. */
static inline struct fct_inner *fct_inner_of(struct fct_outer *o)
{
	return &o->in;
}

int fct_helper_return(struct fct_outer *o)
{
	return fct_inner_of(o)->b;
}

/* include/linux/container_of.h: the member is named in a type check the compiler never evaluates. */
#define container_of(ptr, type, member) ({ \
	void *__mptr = (void *)(ptr); \
	_Static_assert(__builtin_types_compatible_p(typeof(*(ptr)), typeof(((type *)0)->member)), \
		       "pointer type mismatch in container_of()"); \
	((type *)(__mptr - __builtin_offsetof(type, member))); })

/* struct device_link *link = container_of(kref, struct device_link, kref); link->... */
long fct_container_local(struct list_node *n)
{
	struct fct_outer *o = container_of(n, struct fct_outer, link);

	return o->x;
}

/* real_mount(mnt)->mnt_ns. */
static inline struct fct_outer *fct_of(struct list_node *n)
{
	return container_of(n, struct fct_outer, link);
}

long fct_container_helper(struct list_node *n)
{
	return fct_of(n)->x;
}

/* list_for_each_entry(link, &dev->links.consumers, s_node). */
long fct_container_loop(struct list_node *head)
{
	struct fct_outer *o;
	long sum = 0;

	for (o = container_of(head->next, struct fct_outer, link); &o->link != head;
	     o = container_of(o->link.next, struct fct_outer, link))
		sum += o->x;
	return sum;
}

/* A field of a global through a pointer to it. */
int fct_global_field(void)
{
	struct fct_inner *g = &fct_global.in;

	return g->b;
}

/* path_put through __free(path_put): include/linux/path.h names its cleanup by a macro. */
void box_release(struct box *b);
#define __free_box_release box_release

long fct_cleanup_alias(void)
{
	struct box held __free(box_release) = {};

	return held.size;
}

/* drivers/base/cacheinfo.c: a weak default the architecture replaces decides nothing. */
int __weak fct_weak_level(void)
{
	return -1;
}

int fct_after_weak(struct box *b)
{
	if (fct_weak_level())
		return 1;
	obj_free(b);
	return 0;
}

/* kernel/sched/clock.c: notrace unsigned long long __weak sched_clock(void). Its text begins inside
 * a macro of another file, and kernel/time/sched_clock.c gives the definition that wins. */
notrace unsigned long long __weak fct_traced_clock(void)
{
	return 1;
}

int fct_traced_clock_user(void)
{
	return (int)fct_traced_clock();
}

/* The attribute written out, alone and in a list. */
int __attribute__((weak)) fct_direct_weak(void)
{
	return 2;
}

int __attribute__((__noinline__, __weak__)) fct_listed_weak(void)
{
	return 3;
}

/* arch/riscv/include/asm/bug.h and include/linux/compiler.h: BUG() is
 * do { __BUG_FLAGS("", 0); unreachable(); } while (0), and unreachable() is itself
 * do { barrier_before_unreachable(); __builtin_unreachable(); } while (0). kernel/exit.c's abort()
 * calls panic() after BUG(), which is never compiled. */
#define fct_bug_flags() do { __asm__ volatile("ebreak"); } while (0)
#define fct_unreachable() do { __asm__ volatile(""); __builtin_unreachable(); } while (0)
#define FCT_BUG() do { fct_bug_flags(); fct_unreachable(); } while (0)

void fct_after_bug(struct box *b)
{
	FCT_BUG();
	obj_free(b);
}

/* panic() and do_exit() are declared __noreturn. */
void fct_die(void) __attribute__((__noreturn__));

void fct_after_die(struct box *b)
{
	fct_die();
	obj_free(b);
}

/* cpu_to_node(cpu) without NUMA is ((void)(cpu), 0); lockdep_assert_held without lockdep is
 * do { (void)(p); } while (0). The compiler never reads what is discarded. */
#define fct_node_of(p) ((void)(p), 0)

int fct_discarded(struct box *b)
{
	(void)b->size;
	return fct_node_of(b->first);
}

/* A tracepoint stub a macro defines: DECLARE_TRACE's inline functions never read their arguments. */
#define FCT_DEFINE_NOTE(name) static inline void name(struct list_node *n) { }
FCT_DEFINE_NOTE(fct_note)

int fct_ignored_macro(void)
{
	fct_note(all_bags.prev);
	return 0;
}

/* flush_workqueue(): __builtin_constant_p(_wq == system_wq) && _wq == system_wq. The right operand
 * is compiled only where the compiler finds the left one true. */
struct box *fct_slot;

int fct_builtin_and(struct box *b)
{
	if (__builtin_constant_p(b == fct_slot) && b == fct_slot)
		return box_weight();
	return 0;
}

/* kobject_put(&p->kobj), down_write(&nh->rwsem): a member's address, which at offset 0 is the base
 * register itself; an array member decays to its address. */
void fct_member_address(struct fct_outer *o)
{
	obj_free(&o->x);
	obj_free(&o->in);
	obj_free(o->stats);
}

/* arch/riscv/kernel/vdso.c: vdso_info, written once at boot (__ro_after_init), whose initializer
 * stores other globals' addresses; the compiler propagates them into the functions that read it. */
struct fct_info {
	char *code;
	long *start;
	int *inner;
};
char fct_code[16];
long fct_start;
static struct fct_info fct_info __attribute__((section(".data..ro_after_init"))) = {
	.code = fct_code,
	.start = &fct_start,
	.inner = &fct_global.in.b,
};

long *fct_info_start(void)
{
	return fct_info.start;
}

/* bdi_put: wb_exit(&bdi->wb), where wb_exit is folded; its field offsets rebase onto bdi + wb. */
void fct_takes_inner(struct fct_inner *in);

void fct_pass_member(struct fct_outer *o)
{
	fct_takes_inner(&o->in);
}

/* change_clocksource: tk = &tk_core.shadow_timekeeper; the argument is that global's storage. */
void fct_pass_held_global(void)
{
	struct fct_inner *held = &fct_global.in;

	fct_takes_inner(held);
}

/* static_branch_enable(&key) -> static_key_enable(&(x)->key): the global, with its field. */
void fct_pass_arrow_of_address(void)
{
	fct_takes_inner(&(&fct_global)->in);
}

/* __do_adjtimex(tkd) -> __timekeeping_advance(tkd): the parameter itself, or a local equal to it, is
 * the callee's pointer. fsnotify_path(path) -> fsnotify_parent(path->dentry): a pointer loaded
 * through it is not. */
void fct_takes_node(struct list_node *n);

void fct_pass_parameter(struct list_node *n)
{
	struct list_node *same = n;

	fct_takes_node(n);
	fct_takes_node(same);
	fct_takes_node(n->next);
}

/* A parameter the function writes (o = o->next in a walk) is not the pointer it was given. */
long fct_reassigned_parameter(struct fct_outer *o)
{
	long sum = 0;

	while (o) {
		sum += o->x;
		fct_takes_inner(&o->in);
		o = (struct fct_outer *)o->link.next;
	}
	return sum;
}

/* per_cpu_ptr(): ({ __verify_pcpu_ptr(ptr); SHIFT_PERCPU_PTR(ptr, per_cpu_offset(cpu)); }). The last
 * statement is the value, and fct_offsets[cpu] is read. */
long fct_offsets[4];
#define fct_shift(cpu) ({ (void)(cpu); fct_offsets[cpu]; })

long fct_statement_value(int cpu)
{
	return fct_shift(cpu);
}

/* include/linux/signal.h: switch (_NSIG_WORDS), one word on rv64. Only case 1 is compiled. */
#define FCT_WORDS 1

int fct_constant_switch(struct fct_outer *a, struct fct_outer *b)
{
	switch (FCT_WORDS) {
	case 4:
		return a->stats[3] == b->stats[3];
	case 2:
		return a->stats[1] == b->stats[1];
	case 1:
		return a->stats[0] == b->stats[0];
	default:
		box_weight();
	}
	return 0;
}

/* kernel/time/hrtimer.c: __next_base() returns &cpu_base->clock_base[idx]; the register holds the
 * element, so base->offset adds only offset's own offset. */
static inline struct fct_inner *fct_slot_of(struct fct_outer *o, int k)
{
	return &o->slots[k];
}

int fct_indexed_helper(struct fct_outer *o, int k)
{
	struct fct_inner *slot = fct_slot_of(o, k);

	return slot->b + slot->a;
}

/* kernel/printk/internal.h: switch (nbcon_get_default_prio()), a helper with several returns. Its
 * value is not a constant, so every case is code. */
static inline int fct_level(int k)
{
	if (k)
		return 2;
	return 1;
}

long fct_helper_switch(struct fct_outer *a, int k)
{
	switch (fct_level(k)) {
	case 2:
		return a->stats[2];
	case 1:
		return a->stats[1];
	}
	return 0;
}

/* An asm with volatile, or without an output, stays where nothing reads what it gives, and so does
 * its input (llvm-objdump: the addresses of fct_offsets_asm[1] and [2] are formed, [3]'s is not). */
long fct_offsets_asm[4];

static inline void fct_acquire(long *l)
{
	(void)l;
}

void fct_asm_kept(void)
{
	long *one;
	long *two;

	one = ({ unsigned long __p; __asm__ volatile("" : "=r"(__p) : "0"(&fct_offsets_asm[1])); (long *)__p; });
	two = ({ __asm__("" : : "r"(&fct_offsets_asm[2])); &fct_offsets_asm[3]; });
	fct_acquire(one);
	fct_acquire(two);
}

/* prepare_alloc_pages(..., &ac, ...): the inlined helper fills the caller's local object, which the
 * machine code keeps in registers. */
static inline void fct_fill(struct fct_inner *in, int v)
{
	in->a = v;
	in->b = v + 1;
}

int fct_local_object(int v)
{
	struct fct_inner local;

	fct_fill(&local, v);
	return local.a * local.b;
}

/* __clk_release: clk_core_free_parent_map(container_of(ref, struct clk_core, ref)) reads the field
 * at its offset less the member's, from the pointer the caller was given. */
static inline long fct_x_of(struct fct_outer *o)
{
	return o->x;
}

long fct_container_argument(struct list_node *n)
{
	return fct_x_of(container_of(n, struct fct_outer, link));
}

/* __ipi_send_single: data = irq_desc_get_irq_data(desc) is read before data is set again. */
struct fct_inner *fct_other(int k);

int fct_reassigned_local(struct fct_outer *o, int k)
{
	struct fct_inner *d = &o->in;
	int sum = d->a;

	if (k)
		d = fct_other(k);
	return sum + d->b;
}

/* for_each_sched_entity(se) without group scheduling: for (; se; se = NULL). A null pointer is
 * never dereferenced, so se is still &p->se where it is. */
int fct_null_walk(struct fct_outer *o)
{
	struct fct_inner *in = &o->in;
	int sum = 0;

	for (; in; in = NULL)
		sum += in->b;
	return sum;
}

/* A parameter read before the function sets it is still the parameter there. */
int fct_parameter_before_write(struct fct_outer *o)
{
	int a = o->in.a;

	o = &fct_global;
	return a + o->in.b;
}

/* __sb_end_write: percpu_up_read(sb->s_writers.rw_sem + level - 1). The array decays to its address;
 * a constant step folds into the displacement, a variable one is added in a register. */
void fct_pass_step(struct fct_outer *o, int k)
{
	fct_takes_inner(o->slots + 1);
	fct_takes_inner(o->slots + k - 1);
}

/* kernel/time/timekeeping.c: tk_core is timekeeper_data[0], so &tk_core.shadow_timekeeper is the
 * storage of an element of a global array. */
struct fct_outer fct_globals[2];

void fct_pass_array_element(void)
{
	fct_takes_inner(&fct_globals[1].in);
}

/* mm/mm_init.c: NODE_DATA(nid) returns &contig_page_data without NUMA, and pgdat = NODE_DATA(nid)
 * holds that global. */
static inline struct fct_outer *fct_node(int nid)
{
	(void)nid;
	return &fct_global;
}

void fct_pass_helper_global(int nid)
{
	struct fct_outer *pgdat = fct_node(nid);

	obj_free(pgdat);
}

/* to_software_node(fwnode): is_software_node(fwnode) ? container_of(fwnode, ...) : NULL. */
static inline struct fct_outer *fct_maybe(struct list_node *n)
{
	return n->next ? container_of(n, struct fct_outer, link) : NULL;
}

long fct_conditional_container(struct list_node *n)
{
	struct fct_outer *o = fct_maybe(n);

	return o ? o->x : 0;
}

/* for_each_sched_entity with group_cfs_rq(), which returns NULL without group scheduling. */
static inline struct fct_inner *fct_none(struct fct_outer *o)
{
	(void)o;
	return NULL;
}

int fct_null_helper(struct fct_outer *o)
{
	struct fct_inner *in = &o->in;
	int sum = 0;

	for (; in; in = fct_none(o))
		sum += in->b;
	return sum;
}

/* container_of_const(): _Generic on the pointer's constness picks one container_of. */
#define fct_const_of(n) _Generic((n), \
	const struct list_node *: ((const struct fct_outer *)container_of(n, struct fct_outer, link)), \
	struct list_node *: container_of(n, struct fct_outer, link))

long fct_generic_container(struct list_node *n)
{
	return fct_const_of(n)->x;
}

/* INTERVAL_TREE_DEFINE's subtree search: the parameter is set to rb_entry(...) as it walks, and the
 * machine code may keep the member's address instead of the container. */
long fct_walk_entries(struct fct_outer *o, int k)
{
	long sum = 0;

	while (k--) {
		sum += o->x;
		o = container_of(o->link.next, struct fct_outer, link);
	}
	return sum;
}

/* spin_lock_irqsave(&rsp->rss_lock, flags) is raw_spin_lock_irqsave(spinlock_check(lock), flags):
 * the lock is named through an inline helper that returns its address. */
struct fct_locked {
	long pad;
	struct spin lock;
};

static inline struct spin *fct_check(struct spin *s)
{
	return s;
}

void fct_lock_checked(struct fct_locked *l)
{
	spin_lock(fct_check(&l->lock));
}
