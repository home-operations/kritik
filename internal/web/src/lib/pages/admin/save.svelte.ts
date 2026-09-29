// The save flow a spec section shares: the request, the error the form
// shows and focuses, and the reload that follows a save or a conflict.
import { sendJSON } from '../../api.svelte';
import { describe, errorPath, isCode } from '../../manage';
import { toast } from '../../toast.svelte';
import type { ConfigWriteResult, UpdateConfigRequest } from '../../types';

export class SpecSave {
  saving = $state(false);
  dirty = $state(false);
  errMessage = $state('');
  errPath = $state('');
  errSeq = $state(0);
  conflict = $state(false);
  // Bumped to remount the editor on a fresh draft.
  epoch = $state(0);

  // path is where the spec is PUT, and load reads it back.
  constructor(
    private readonly path: () => string,
    private readonly load: () => Promise<void>,
  ) {}

  async reload(): Promise<void> {
    this.clearError();
    this.dirty = false;
    await this.load();
    this.epoch++;
  }

  // put saves body and reloads. saved sees the write's result before the
  // reload; refused may claim a refusal before the form shows it.
  async put(
    body: UpdateConfigRequest,
    on: { saved?: (r: ConfigWriteResult) => void; refused?: (err: unknown) => boolean } = {},
  ): Promise<void> {
    this.saving = true;
    this.clearError();
    try {
      const r = await sendJSON<ConfigWriteResult>('PUT', this.path(), body);
      toast(`Saved: revision ${r.revision}`);
      on.saved?.(r);
      await this.reload();
    } catch (err) {
      if (!on.refused?.(err)) {
        this.errMessage = describe(err);
        this.errPath = errorPath(err);
        this.errSeq++;
        this.conflict = isCode(err, 'revision_conflict');
      }
    } finally {
      this.saving = false;
    }
  }

  private clearError(): void {
    this.errMessage = '';
    this.errPath = '';
    this.conflict = false;
  }
}
