/*
 * Copyright 2026 The Kubernetes Authors
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

import { ApiError } from './ApiError';
import { getDiscoveredConnectionRevision } from './discoveredConnectionRevisions';

export const UNVERSIONED_DISCOVERY_CONNECTION = 'unversioned';
export const DISCOVERY_CONNECTION_REVISION_PARAM = '__headlamp_connection_revision';
export const DISCOVERY_CONNECTION_REVISION_HEADER = 'X-Headlamp-Connection-Revision';

export const DISCOVERY_CONNECTION_CHANGED = 'Discovered cluster connection changed';

export function isDiscoveryConnectionChanged(error: unknown) {
  return (
    error instanceof Error &&
    (error as ApiError).status === 409 &&
    error.message.endsWith(DISCOVERY_CONNECTION_CHANGED)
  );
}

export function createDiscoveryConnectionChangedError() {
  return new ApiError(DISCOVERY_CONNECTION_CHANGED, { status: 409 });
}

export interface DiscoveryConnectionRequest {
  revision?: string;
}

const connectionRequest = Symbol('discoveryConnectionRequest');
type ResourceConnection = { cluster: string; revision?: string };
const resourceConnection = Symbol('resourceConnection');
const frozenResourceConnections = new WeakMap<object, ResourceConnection>();

export function assertCurrentDiscoveryConnection(cluster: string | undefined, revision?: string) {
  if (cluster && getDiscoveredConnectionRevision(cluster) !== revision) {
    throw createDiscoveryConnectionChangedError();
  }
}

export function recordDiscoveryConnection(resource: object, cluster: string, revision?: string) {
  if (!cluster) return;
  const origin = { cluster, revision };
  if (Object.isExtensible(resource)) {
    // Object spread and cloneDeep retain the origin. Kubernetes JSON does not include symbols.
    Object.defineProperty(resource, resourceConnection, {
      value: origin,
      enumerable: true,
      configurable: true,
    });
  } else {
    frozenResourceConnections.set(resource, origin);
  }
}

export function getResourceDiscoveryConnection(resource: object) {
  return (
    frozenResourceConnections.get(resource) ??
    (resource as { [resourceConnection]?: ResourceConnection })?.[resourceConnection]
  );
}

// Plugins can check permissions on a different ordinary cluster. A resource from a
// discovered or imported connection can only check permissions on its own revision.
export function canCheckResourceAuthorization(
  resource: object,
  cluster: string | null | undefined,
  revision?: string
) {
  const origin = getResourceDiscoveryConnection(resource);
  if (!origin) return true;
  if (origin.cluster === cluster) return origin.revision === revision;
  return (
    isUnversionedConnection(origin.revision) &&
    isUnversionedConnection(revision) &&
    getDiscoveredConnectionRevision(origin.cluster) === origin.revision
  );
}

export function inheritResourceConnection<T extends object>(target: T, source: object): T {
  const origin = getResourceDiscoveryConnection(source);
  if (origin) recordDiscoveryConnection(target, origin.cluster, origin.revision);
  return target;
}

/** The part of a local revision (`<routing>:<epoch>`) that the backend checks. */
export function getRoutingRevision(revision?: string) {
  return revision?.split(':', 1)[0];
}

export function isUnversionedConnection(revision?: string | null) {
  return (
    revision === null ||
    revision === undefined ||
    getRoutingRevision(revision) === UNVERSIONED_DISCOVERY_CONNECTION
  );
}

/** Attach the revision of the route that owns this request. */
export function withDiscoveryConnectionRevision<T extends object>(params: T, revision?: string) {
  const routingRevision = getRoutingRevision(revision);
  return routingRevision ? { ...params, __headlamp_connection_revision: routingRevision } : params;
}

/** Pin a path supplied by a low-level API caller, preserving an explicitly selected route. */
export function withDiscoveryConnectionPath(path: string, connection?: DiscoveryConnectionRequest) {
  if (!connection) return path;
  const routingRevision =
    connection.revision === undefined
      ? UNVERSIONED_DISCOVERY_CONNECTION
      : getRoutingRevision(connection.revision);
  if (!routingRevision) return path;

  const queryIndex = path.indexOf('?');
  const param = DISCOVERY_CONNECTION_REVISION_PARAM;
  if (queryIndex >= 0 && new URLSearchParams(path.slice(queryIndex + 1)).has(param)) return path;

  const separator = queryIndex < 0 ? '?' : '&';
  return path + separator + new URLSearchParams({ [param]: routingRevision });
}

/** Carry the local revision through async request setup without sending it on the wire. */
export function bindDiscoveryConnection<T extends object>(params: T, revision?: string) {
  const query = withDiscoveryConnectionRevision({ ...params }, revision);
  Object.defineProperty(query, connectionRequest, { value: { revision } });
  return query;
}

export function getDiscoveryRequestBinding(params?: object) {
  return (params as { [connectionRequest]?: DiscoveryConnectionRequest } | undefined)?.[
    connectionRequest
  ];
}

export function discoveryRequestOptions(cluster?: string, params?: object) {
  const expectedDiscoveryConnection = getDiscoveryRequestBinding(params);
  return expectedDiscoveryConnection ? { cluster, expectedDiscoveryConnection } : { cluster };
}

export function getResourceConnectionQuery<T extends object>(
  resource: object,
  cluster: string,
  params: T
) {
  const connection = getResourceDiscoveryConnection(resource);
  if (!connection) return params;
  if (connection.cluster !== cluster) throw createDiscoveryConnectionChangedError();
  assertCurrentDiscoveryConnection(cluster, connection.revision);
  return bindDiscoveryConnection(params, connection.revision);
}
