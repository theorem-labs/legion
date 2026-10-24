/* Only src/flow.c includes this file, as only kernel/rcu/tree.c includes tree_stall.h: its external
 * definitions are flow.c's own functions, and its statics are compiled into flow.c alone. */
#ifndef FLOW_PART_H
#define FLOW_PART_H

/* An external function nothing in the unit calls, as tree_stall.h's rcu_fwd_progress_check. */
int flow_header_external(void)
{
	return all_bags.prev != NULL;
}

/* A static that only this file's table names, as rcu_panic in tree_stall.h's notifier block. */
static int flow_header_notify(struct box *b)
{
	return b->size > 2;
}

struct ops flow_header_ops = {
	.get = flow_header_notify,
};

#endif
