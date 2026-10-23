/* One auipc anchor addresses two globals: the second load is an offset from the first. */
	.text
	.globl	anchor_pair
	.type	anchor_pair, @function
anchor_pair:
1:	auipc	a5, %pcrel_hi(pair_a)
	addi	a5, a5, %pcrel_lo(1b)
	ld	a0, 0(a5)
	ld	a1, 8(a5)
	add	a0, a0, a1
	ret
	.size	anchor_pair, .-anchor_pair

	.data
	.balign	8
	/* A zero-size name at the address of pair_a, as a linker script alias gives one. */
	.globl	pair_alias
	.type	pair_alias, @object
	.size	pair_alias, 0
pair_alias:
	.globl	pair_a
	.type	pair_a, @object
	.size	pair_a, 8
pair_a:
	.dword	1
	.globl	pair_b
	.type	pair_b, @object
	.size	pair_b, 8
pair_b:
	.dword	2
