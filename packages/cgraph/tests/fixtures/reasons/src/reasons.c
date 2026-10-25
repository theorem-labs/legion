/* State accesses the graph cannot hold, each kept in exclusions.json with its reason (contract
 * C3): a lock that is the function's own parameter (parameter_at_root), and a lock a folded callee
 * takes through a pointer its caller cannot name (unbound_parameter). A lock reached through a
 * member of the caller's parameter is held: it is an edge, not an exclusion. */

void spin_lock(void *lock);
void spin_unlock(void *lock);

struct guard {
	long count;
};

struct holder {
	struct guard *outer;
	long tag;
};

struct guard *fetch_guard(void);

/* The lock this function takes is its own parameter: no state of the program names it. */
long take_parameter(struct guard *g, long value)
{
	spin_lock(g);
	spin_unlock(g);
	return value + 1;
}

static inline void take_inner(struct guard *g)
{
	spin_lock(g);
	spin_unlock(g);
}

/* The folded callee's lock is a member of this caller's parameter: the graph holds it. */
long take_loaded(struct holder *h)
{
	take_inner(h->outer);
	return h->tag;
}

/* The folded callee's lock is the value of a call: the caller binds its parameter to nothing. */
long take_fetched(void)
{
	take_inner(fetch_guard());
	return 0;
}
