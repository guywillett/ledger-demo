import { readFileSync } from 'node:fs';
import { Ajv2020, type ValidateFunction } from 'ajv/dist/2020.js';
import addFormatsModule from 'ajv-formats';
import { parse } from 'yaml';

// Checks responses against contracts/openapi.yaml, the hand-written, language-neutral
// contract. OpenAPI 3.1 schemas are JSON Schema 2020-12, so Ajv can validate them once the
// "#/components/schemas/…" references are pointed at a schema document Ajv knows.

type Operation = {
  responses: Record<string, { content?: Record<string, { schema: unknown }> } | undefined>;
};
type OpenApi = {
  paths: Record<string, Record<string, Operation | undefined> | undefined>;
  components: { schemas: Record<string, unknown> };
};

const contract = parse(
  readFileSync(new URL('../../contracts/openapi.yaml', import.meta.url), 'utf8'),
) as OpenApi;

const SCHEMAS_ID = 'urn:ledger-demo:contract';

function retarget(schema: unknown): unknown {
  return JSON.parse(
    JSON.stringify(schema).replaceAll('"#/components/schemas/', `"${SCHEMAS_ID}#/$defs/`),
  );
}

// ajv-formats is CommonJS with a `default` property on its export. Importing CommonJS from
// ESM gives the whole module.exports as the default import, and loaders (Node, Vitest) differ
// in whether they unwrap `default`, so accept either shape.
const addFormats = (
  'default' in addFormatsModule ? addFormatsModule.default : addFormatsModule
) as (ajv: Ajv2020) => Ajv2020;

const ajv = new Ajv2020({ allErrors: true });
addFormats(ajv);
ajv.addSchema({ $id: SCHEMAS_ID, $defs: retarget(contract.components.schemas) });

// Ajv caches compiled schemas by object identity, and retarget() returns a new object each
// time, so compile each response schema once and keep the validator.
const validators = new Map<string, ValidateFunction>();

function validatorFor(key: string, schema: unknown): ValidateFunction {
  let validate = validators.get(key);
  if (validate === undefined) {
    validate = ajv.compile(retarget(schema) as object);
    validators.set(key, validate);
  }
  return validate;
}

type InjectedResponse = {
  statusCode: number;
  headers: Record<string, string | string[] | number | undefined>;
  body: string;
};

// Throws unless the contract documents this status and content type for the operation and
// the body matches its schema.
export function expectMatchesContract(
  method: string,
  path: string,
  response: InjectedResponse,
): void {
  const operation = contract.paths[path]?.[method.toLowerCase()];
  if (operation === undefined) throw new Error(`contract has no ${method} ${path}`);

  const documented = operation.responses[String(response.statusCode)];
  if (documented === undefined) {
    throw new Error(`contract has no ${response.statusCode} response for ${method} ${path}`);
  }
  // A response documented without content (e.g. 204) must have no body.
  if (documented.content === undefined) {
    if (response.body !== '') {
      throw new Error(
        `contract has no body for ${response.statusCode} on ${method} ${path}, ` +
          `got: ${response.body}`,
      );
    }
    return;
  }
  const contentType = String(response.headers['content-type'] ?? '').split(';')[0] ?? '';
  const media = documented.content?.[contentType];
  if (media === undefined) {
    throw new Error(
      `contract has no ${contentType} body for ${response.statusCode} on ${method} ${path}`,
    );
  }

  const validate = validatorFor(
    `${method} ${path} ${response.statusCode} ${contentType}`,
    media.schema,
  );
  if (!validate(JSON.parse(response.body))) {
    throw new Error(
      `${method} ${path} ${response.statusCode} body does not match the contract: ` +
        ajv.errorsText(validate.errors) +
        `\nbody: ${response.body}`,
    );
  }
}

export function contractRequestExample(method: string, path: string): unknown {
  const operation = contract.paths[path]?.[method.toLowerCase()] as
    { requestBody?: { content: Record<string, { example?: unknown }> } } | undefined;
  return operation?.requestBody?.content['application/json']?.example;
}

// Validation errors for `value` against one property of a component schema, e.g.
// ('WithdrawalRequest', 'creditor_iban'), or undefined if it's valid. For checking that what the
// implementation accepts, the contract accepts too (test/vectors.test.ts).
export function contractPropertyErrors(
  schemaName: string,
  property: string,
  value: unknown,
): string | undefined {
  const schema = contract.components.schemas[schemaName] as
    { properties?: Record<string, unknown> } | undefined;
  const propertySchema = schema?.properties?.[property];
  if (propertySchema === undefined) {
    throw new Error(`contract has no ${schemaName}.${property}`);
  }
  const validate = validatorFor(`${schemaName}.${property}`, propertySchema);
  return validate(value) ? undefined : ajv.errorsText(validate.errors);
}

// Event contracts: contracts/events/<event_type>.v<version>.json, plain JSON Schema 2020-12.
const eventValidators = new Map<string, ValidateFunction>();

// Throws unless `event` matches the contract for its event_type and schema_version.
export function expectEventMatchesContract(event: unknown): void {
  const { event_type: type, schema_version: version } = event as {
    event_type?: unknown;
    schema_version?: unknown;
  };
  const key = `${String(type)}.v${String(version)}`;
  let validate = eventValidators.get(key);
  if (validate === undefined) {
    const schema = JSON.parse(
      readFileSync(new URL(`../../contracts/events/${key}.json`, import.meta.url), 'utf8'),
    ) as object;
    validate = ajv.compile(schema);
    eventValidators.set(key, validate);
  }
  if (!validate(event)) {
    throw new Error(
      `${key} event does not match the contract: ${ajv.errorsText(validate.errors)}\n` +
        `event: ${JSON.stringify(event)}`,
    );
  }
}
