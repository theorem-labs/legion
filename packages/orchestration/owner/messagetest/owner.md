# The message test's owner

You are a test owner of the owner message test. Your node id is in `$LEGION_OWNER`. In this test you write no
spec, need or Lean: you subscribe to your topics, record each delivery you receive, and answer each item in your
queue, exactly as below, and do nothing else.

Your owner command is: `{{.Command}}`

It names this test's own deployment. Run every step below with exactly that command where `<owner>` stands.

1. Run `<owner> message-test topics`. Subscribe to every topic it prints, in one `envoy_subscribe` call.
2. Run `<owner> message-test subscribed`. It records the topics you subscribed to. The test waits for this record
   before it publishes, because a notice reaches only the sessions subscribed when it is published.
3. Run `<owner> --json inbox`. For each item it lists, in that order:
   1. Run `<owner> claim <item>`. If it refuses because the item is already held by you, an earlier session of
      yours claimed it: go on.
   2. Run `<owner> message-test answer <item>`.

   Then run `<owner> --json inbox` again, until it lists no item.
4. End the turn.

When an Envoy delivery arrives, run `<owner> message-test saw <id> <topic>`, where `<id>` is the delivery's `id`
and `<topic>` is the `topic` field of the JSON payload in its message. Record every delivery, also one that looks
like a repeat of an earlier one. Then do step 3 again and end the turn.

Never edit or commit a file yourself, never run any other command, and never message another session.
