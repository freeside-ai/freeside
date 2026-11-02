-- Manual submission task lines (plan §5.4; issue #1939): the per-role agent
-- choices a submission carried, so admission can hold a cli_submit line to the
-- command that set it.
--
-- task_lines is the JSON list of domain.TaskLineChoice in canonical role
-- order, NULL when the request carried none. Every row stored before this
-- migration reads as having none, even if its request carried lines: the
-- request digest covers them but cannot be reversed. Admission refuses a line
-- that cites such a row, and `freesided task-line` recovers the task.
ALTER TABLE manual_submissions ADD COLUMN task_lines TEXT;
