# Image viewing

`dashy view PATH...` reads PNG or JPEG images, captures them, and sends them to the running session
through `DASHY_ENDPOINT` (or `-s SESSION_ID` outside a dashy shell). Relative paths resolve against
the directory of the command. Because it is an ordinary command, it works from scripts and background jobs.

Limits: inputs up to 32 MiB and 20 megapixels. Every image is prepared as a preview of at most
2048 pixels on its longest side and 1 MiB of encoded data. Aspect ratio is kept, and small images are
not enlarged. Transparent PNGs keep their transparency. Opaque PNGs may become JPEGs to fit the byte
budget. At most four images may wait for one model request; more fail until the queue is delivered.
Invalid files fail with a nonzero exit status.

Delivery: queued images are attached as image content in a separate user notification just before the
next model request, after all tool outputs of the current response. They do not start a turn. Combine
`dashy view` with `dashy notify` when a background job should wake the agent.

Originals are never modified. An exact copy is saved with private permissions: under
`$DASHY_HOME/attachments` for `dashy view` (`attachment_dir` does not apply to it), and for headless
`images` under `attachment_dir` when it is set, else `$DASHY_HOME/attachments`. Its absolute path
is included in the model context, so the agent can crop a detail and call `dashy view` again. Originals stay on disk for manual cleanup. Only the previews are embedded in API
history, so restoring a session does not need the originals. Existing history is not resized.

Images can make session files larger and may contain sensitive data. The provider must accept image
input. These limits do not override provider limits on image count or total request size. Pasted
images follow the same rules; see `dashy docs clipboard`. The terminal never renders image pixels.
