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

import {
  getRoutingRevision,
  isUnversionedConnection,
  UNVERSIONED_DISCOVERY_CONNECTION,
} from '../../lib/k8s/api/v2/discoveryConnection';

export const AUTH_STATUS_KEY = 'auth_status';

// The backend omits the revision from ordinary OIDC callbacks.
export function oidcSessionRevision(revision?: string | null) {
  return isUnversionedConnection(revision) ? undefined : getRoutingRevision(revision ?? undefined);
}

export function oidcAuthStatusKey(cluster: string | null, revision: string | null) {
  return cluster && revision && revision !== UNVERSIONED_DISCOVERY_CONNECTION
    ? `${AUTH_STATUS_KEY}.${encodeURIComponent(cluster)}.${revision}`
    : AUTH_STATUS_KEY;
}
