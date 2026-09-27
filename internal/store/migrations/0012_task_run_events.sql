-- A task run's new row and each status change reach the dashboard's live
-- views like a review's (see kritik_notify_event in 0004_web.sql).
CREATE TRIGGER kritik_notify_task_run_insert
    AFTER INSERT ON task_runs
    FOR EACH ROW
    EXECUTE FUNCTION kritik_notify_event('task_run');

CREATE TRIGGER kritik_notify_task_run_update
    AFTER UPDATE ON task_runs
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION kritik_notify_event('task_run');
