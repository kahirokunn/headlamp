/*
 * Copyright 2025 The Kubernetes Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import type { PortForward } from '../../lib/k8s/api/v1/portForward';

export const PORT_FORWARDS_STORAGE_KEY = 'portforwards';

export function getPortForwardsFromStorage(): PortForward[] {
  try {
    const portForwardsInStorage = localStorage.getItem(PORT_FORWARDS_STORAGE_KEY);
    if (!portForwardsInStorage) {
      return [];
    }
    const parsed = JSON.parse(portForwardsInStorage);
    if (!Array.isArray(parsed)) {
      return [];
    }
    return parsed.filter((pf: unknown): pf is PortForward => {
      if (pf === null || typeof pf !== 'object' || Array.isArray(pf)) {
        return false;
      }
      const r = pf as Record<string, unknown>;
      return (
        typeof r.id === 'string' &&
        typeof r.pod === 'string' &&
        typeof r.service === 'string' &&
        typeof r.serviceNamespace === 'string' &&
        typeof r.namespace === 'string' &&
        typeof r.cluster === 'string' &&
        typeof r.port === 'string' &&
        typeof r.targetPort === 'string' &&
        (r.status === undefined || typeof r.status === 'string') &&
        (r.error === undefined || typeof r.error === 'string')
      );
    });
  } catch (err) {
    console.warn('Failed to parse port forwards from storage', err);
    return [];
  }
}

export function samePortForward(first: PortForward, second: PortForward) {
  return (
    first.id === second.id &&
    first.cluster === second.cluster &&
    (first.connectionRevision || undefined) === (second.connectionRevision || undefined)
  );
}

export function removeSavedPortForward(forward: PortForward): Error | null {
  try {
    localStorage.setItem(
      PORT_FORWARDS_STORAGE_KEY,
      JSON.stringify(getPortForwardsFromStorage().filter(saved => !samePortForward(saved, forward)))
    );
    return null;
  } catch (error) {
    return error instanceof Error ? error : new Error(String(error));
  }
}
