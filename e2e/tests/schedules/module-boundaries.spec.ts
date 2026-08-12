import { test, expect } from '@playwright/test';
import * as fs from 'fs';
import * as path from 'path';
import * as ts from 'typescript';

/**
 * The shape of the schedules feature, checked rather than agreed.
 *
 * The feature was one file of sixteen hundred lines, and splitting it is only
 * worth anything if it stays split. What keeps it split is this: the graph is
 * directed, and the file at the top of it does nothing but wire.
 *
 * The graph is read from the syntax tree, not from the text. A search for
 * "import" misses the multi-line form, disagrees with itself about quotes, and
 * knows nothing about `export ... from` or a dynamic `import()` - and every
 * one of those is a way for a module to reach a module it must not.
 *
 * These tests need no browser. They are here because this is where the
 * TypeScript compiler already lives, and because a boundary that is only
 * checked in review is a boundary that lasts about a month.
 */

const JS_ROOT = path.resolve(__dirname, '../../../web/js');
const MODULES = path.join(JS_ROOT, 'modules');

const ROOT_MODULE = 'schedules.js';
const FEATURE_MODULES = [
  'schedule-overview.js',
  'schedule-editor.js',
  'schedule-overrides.js',
  'schedule-calendar.js',
];
const SHARED_MODULE = 'schedule-shared.js';
const COMPONENTS_MODULE = 'schedule-components.js';

type Edge = { spec: string; kind: 'import' | 'export' | 'dynamic' };

function parse(file: string): ts.SourceFile {
  return ts.createSourceFile(
    file, fs.readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);
}

/** Every module this file reaches, however it reaches it. */
function edgesOf(file: string): Edge[] {
  const edges: Edge[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isImportDeclaration(node) && ts.isStringLiteral(node.moduleSpecifier)) {
      edges.push({ spec: node.moduleSpecifier.text, kind: 'import' });
    } else if (ts.isExportDeclaration(node) && node.moduleSpecifier
      && ts.isStringLiteral(node.moduleSpecifier)) {
      edges.push({ spec: node.moduleSpecifier.text, kind: 'export' });
    } else if (ts.isCallExpression(node)
      && node.expression.kind === ts.SyntaxKind.ImportKeyword
      && node.arguments.length > 0 && ts.isStringLiteral(node.arguments[0])) {
      edges.push({ spec: (node.arguments[0] as ts.StringLiteral).text, kind: 'dynamic' });
    }
    ts.forEachChild(node, visit);
  };
  visit(parse(file));
  return edges;
}

function jsFilesUnder(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) return jsFilesUnder(full);
    return entry.isFile() && entry.name.endsWith('.js') ? [full] : [];
  });
}

/** Which of the named modules an edge points at, if any. */
function targets(edges: Edge[], names: string[]): Edge[] {
  return edges.filter(edge => names.includes(path.basename(edge.spec)));
}

function count(file: string, predicate: (node: ts.Node) => boolean): number {
  let found = 0;
  const visit = (node: ts.Node) => {
    if (predicate(node)) found += 1;
    ts.forEachChild(node, visit);
  };
  visit(parse(file));
  return found;
}

const readsGlobal = (name: string) => (node: ts.Node) =>
  ts.isPropertyAccessExpression(node)
  && ts.isIdentifier(node.expression)
  && node.expression.text === name;

const assignsInnerHtml = (node: ts.Node) =>
  ts.isBinaryExpression(node)
  && node.operatorToken.kind === ts.SyntaxKind.EqualsToken
  && ts.isPropertyAccessExpression(node.left)
  && node.left.name.text === 'innerHTML';

const callsOn = (object: string, method: string) => (node: ts.Node) =>
  ts.isCallExpression(node)
  && ts.isPropertyAccessExpression(node.expression)
  && node.expression.name.text === method
  && ts.isIdentifier(node.expression.expression)
  && node.expression.expression.text === object;

/** Bindings declared beside the module rather than inside a call. */
function moduleLevelMutables(file: string): string[] {
  return parse(file).statements
    .filter(ts.isVariableStatement)
    .filter(statement => (statement.declarationList.flags & ts.NodeFlags.Const) === 0)
    .flatMap(statement => statement.declarationList.declarations.map(d => d.name.getText()));
}

test.describe('schedule module boundaries', () => {
  test('only the composition root imports the feature modules', () => {
    const offenders: string[] = [];

    for (const file of jsFilesUnder(JS_ROOT)) {
      if (path.basename(file) === ROOT_MODULE) continue;
      const reaching = targets(edgesOf(file), FEATURE_MODULES);
      for (const edge of reaching) {
        offenders.push(`${path.relative(JS_ROOT, file)} -> ${edge.spec} (${edge.kind})`);
      }
    }

    expect(offenders,
      'a feature module reached from outside the root is a graph with two owners').toEqual([]);
  });

  test('feature modules know nothing of each other, or of the root', () => {
    const offenders: string[] = [];

    for (const name of FEATURE_MODULES) {
      const edges = edgesOf(path.join(MODULES, name));
      const siblings = FEATURE_MODULES.filter(other => other !== name);
      for (const edge of targets(edges, [...siblings, ROOT_MODULE])) {
        offenders.push(`${name} -> ${edge.spec} (${edge.kind})`);
      }
    }

    // Checked for all four at once on purpose: a test that looks at one module
    // passes while another one grows the edge that closes the cycle.
    expect(offenders,
      'calendar and overrides reach each other through callbacks, not imports').toEqual([]);
  });

  test('the shared module holds values, not the feature', () => {
    const file = path.join(MODULES, SHARED_MODULE);

    expect(targets(edgesOf(file), [...FEATURE_MODULES, ROOT_MODULE, COMPONENTS_MODULE]),
      'shared code that imports the feature is the feature').toEqual([]);
    expect(count(file, readsGlobal('API')),
      'a request here would make this the place flows reassemble').toBe(0);
    expect(moduleLevelMutables(file),
      'state beside a shared module outlives every screen that reads it').toEqual([]);
  });

  test('the root wires, and does nothing else', () => {
    const file = path.join(MODULES, ROOT_MODULE);
    const edges = edgesOf(file);

    // A directed graph does not stop the file at the top of it from keeping a
    // thousand lines of what it was. Its role is a list, and this is the list.
    expect(count(file, readsGlobal('API')), 'the root makes no requests').toBe(0);
    expect(count(file, readsGlobal('Components')), 'the root renders nothing').toBe(0);
    expect(count(file, assignsInnerHtml), 'the root writes no markup').toBe(0);
    expect(count(file, callsOn('document', 'addEventListener')),
      'one listener, for the life of the page').toBe(1);
    expect(moduleLevelMutables(file), 'the root remembers nothing between calls').toEqual([]);

    const componentEdges = targets(edges, [COMPONENTS_MODULE]);
    expect(componentEdges.map(edge => edge.kind),
      'the root passes the one public component through, it does not draw with it')
      .toEqual(['export']);
  });
});
