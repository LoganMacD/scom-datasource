import { useCallback, useState } from 'react';

/** The message to show for a failed resource request. The backend answers with {"error": "..."}. */
export function loadErrorMessage(err: unknown): string {
  const e = err as { data?: { error?: string; message?: string }; message?: string; statusText?: string } | undefined;
  return e?.data?.error || e?.data?.message || e?.message || e?.statusText || 'The request failed';
}

/**
 * Remembers why a picker's options last failed to load, so the field can show
 * the reason. The Combobox on its own only says "An error occurred while
 * loading options".
 */
export function useLoadError() {
  const [error, setError] = useState<string>();
  const track = useCallback(
    <T>(promise: Promise<T>): Promise<T> =>
      promise.then(
        (value) => {
          setError(undefined);
          return value;
        },
        (err: unknown) => {
          setError(loadErrorMessage(err));
          throw err;
        }
      ),
    []
  );
  return { error, track };
}
