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
