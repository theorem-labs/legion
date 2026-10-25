/* The compiler removes the read of s->mark (it is subtracted from itself), and the load the
 * function keeps reads the second parameter at the same displacement, 192. A field offset must be
 * confirmed by a load or store through the object the C names, not by any load with that number. */

struct slot {
	char pad[192];
	long mark;
};

long read_mark(struct slot *s, const long *other, long n)
{
	return (s->mark - s->mark) + other[24] + n;
}

long root(struct slot *s, const long *other, long n)
{
	return read_mark(s, other, n);
}
