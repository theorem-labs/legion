#include "mini.h"

/* What the compiler removes before it emits code: a branch an inline helper's constant decides, the
 * right operand of a decided &&, the code after a return, a __builtin_constant_p choice. */

static inline int feature_on(void)
{
	return 0;
}

int flow_guarded(struct box *b)
{
	int on = feature_on();

	if (on && b->size)
		return box_weight();
	if (feature_on())
		obj_free(b);
	return 0 && bag_weight();
}

int flow_after_return(struct box *b)
{
	return 1;
	obj_free(b);
}

static inline int pick(long n)
{
	return __builtin_constant_p(n) ? (int)n : box_weight();
}

int flow_builtin(void)
{
	return pick(4);
}

/* A container_of: the item's field is reached from its link member. */
#define item_of(p) ((struct item *)((char *)(p) - __builtin_offsetof(struct item, link)))

int flow_container(struct list_node *n)
{
	return item_of(n)->value;
}

/* A value only a parameter the inline callee never reads receives: the compiler never computes it. */
static inline void note_bag(struct list_node *n)
{
}

int flow_ignored(void)
{
	note_bag(all_bags.next);
	return 0;
}

/* After `if (__builtin_constant_p(n)) return`, the rest is compiled only for a non-constant n. */
static inline int pick_exit(long n)
{
	if (__builtin_constant_p(n))
		return (int)n;
	return box_weight();
}

int flow_builtin_exit(void)
{
	return pick_exit(4);
}

/* A function passed as a pointer to an inline helper that calls it: the compiler inlines both. */
static inline int apply(int (*fn)(void))
{
	return fn();
}

static int read_bags(void)
{
	return all_bags.next != NULL;
}

int flow_pointer(void)
{
	return apply(read_bags);
}


static inline int feature_enabled(void)
{
	return 1;
}

int flow_decided_or(void)
{
	return feature_enabled() || box_weight();
}

int flow_constant_or(void)
{
	return 1 || bag_weight();
}

int flow_after_goto(struct box *b)
{
	goto done;
	obj_free(b);
done:
	return 0;
}

int flow_after_trap(struct box *b)
{
	__builtin_trap();
	obj_free(b);
	return 0;
}

static int box_count;

int flow_alignof(void)
{
	return __alignof__(box_count);
}

struct anonymous_holder {
	union {
		struct {
			int ignored;
			int b;
		};
	};
};

int flow_anonymous_member(struct anonymous_holder *s)
{
	return s->b;
}

static int partial_helper(void)
{
	all_bags.next = all_bags.prev;
	return all_bags.next != NULL;
}

int flow_partial_inline(void)
{
	[[clang::always_inline]] partial_helper();
	[[clang::noinline]] return partial_helper();
}

int flow_percpu __attribute__((section(".data..percpu"))) = 1;

int flow_percpu_global(void)
{
	return flow_percpu++;
}

struct percpu_box {
	int value;
};

int flow_percpu_struct(struct percpu_box *value)
{
	return value->value;
}


int flow_nested_generic(struct box *value)
{
	return outer_weight(value);
}
