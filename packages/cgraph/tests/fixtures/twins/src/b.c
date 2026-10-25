/* The other unit defining `tally`: three parameters, the last the address of a global this unit
 * passes. A decoder that reads the other unit's one parameter sees no third argument, and the
 * address of that global then escapes nowhere: the access through it is lost. */

struct right {
	long first;
	long second;
};

struct right right_state;
struct right right_other;

static __attribute__((noinline)) long tally(long a, long b, struct right *r)
{
	return a + b + r->second;
}

long (*volatile right_hook)(long, long, struct right *) = tally;

long right_entry(long k)
{
	return tally(k, 2, &right_state);
}

long right_entry_other(long k)
{
	return tally(k, 3, &right_other);
}
