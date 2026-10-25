/* State accesses the graph cannot hold, each kept in exclusions.json with its reason (contract
 * C3): a lock that is the function's own parameter (parameter_at_root), a lock a folded callee
 * takes through a pointer its caller cannot name (unbound_parameter), and a field read in a
 * _Generic association whose selection the facts could not read (generic_unconfirmed). A lock
 * reached through a member of the caller's parameter is held: it is an edge, not an exclusion. */

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

struct common {
	long state;
};

struct ring {
	struct common ns;
	long head;
};

struct other {
	struct common ns;
	long tail;
};

/* As the kernel's ns_tree_active(): a _Generic whose associations are all the same member of a
 * different struct, inside the argument of another macro. Every association has the type of the
 * whole, so the type tells nothing, and the facts read no selection from the text either: the
 * access through it is kept with its reason, not held. */
#define EMPTY_STATE(member) ((member) == 0)
#define to_common(p)                                                                           \
	_Generic((p), struct ring *: &(p)->ns, const struct ring *: &(p)->ns, struct other *: &(p)->ns, \
		 const struct other *: &(p)->ns)
#define ring_active(p) (!EMPTY_STATE(to_common(p)->state))

long active_ring(struct ring *r)
{
	return ring_active(r);
}
