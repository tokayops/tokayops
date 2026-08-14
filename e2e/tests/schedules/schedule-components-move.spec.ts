import { test, expect } from '../../fixtures/auth.fixture';
import { waitForAppReady } from '../../pages/app.utils';
import * as fs from 'fs';
import * as path from 'path';
import * as ts from 'typescript';

/**
 * The schedule markup left the app-wide component object, and left it whole.
 *
 * "Moved" is easy to half-do: the function goes to its new home and the old
 * one keeps a copy, or a bridge is left in the global "for now" and the
 * boundary exists only in the imports. Either way the file that was supposed
 * to lose a sub-domain still answers for it.
 *
 * So both halves are checked - the definitions are gone from the source, and
 * the object the page still puts on `window` does not carry them.
 */

const COMPONENTS_FILE = path.resolve(__dirname, '../../../web/js/components.js');

/** The thirteen, by name. A move is provable only if the list is. */
const MOVED = [
  'onCallWidget',
  'scheduleState',
  'onCallStatus',
  'onCallNames',
  'assignmentTimes',
  'onCallListHeader',
  'onCallOverviewRow',
  'scheduleWarnings',
  'scheduleConfigModal',
  'overridesList',
  'overrideModal',
  'monthlyScheduleCalendar',
  'schedulePreview',
];

/** The properties of the `Components` object literal, read from the syntax. */
function componentNames(): string[] {
  const source = ts.createSourceFile(
    COMPONENTS_FILE, fs.readFileSync(COMPONENTS_FILE, 'utf8'),
    ts.ScriptTarget.Latest, true, ts.ScriptKind.JS);

  const names: string[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isVariableDeclaration(node)
      && ts.isIdentifier(node.name) && node.name.text === 'Components'
      && node.initializer && ts.isObjectLiteralExpression(node.initializer)) {
      for (const property of node.initializer.properties) {
        if (property.name && ts.isIdentifier(property.name)) names.push(property.name.text);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return names;
}

test.describe('schedule components moved out of the global', () => {
  test('components.js defines none of the thirteen', () => {
    const defined = componentNames();
    expect(defined.length, 'the Components object was not found').toBeGreaterThan(0);
    expect(defined.filter(name => MOVED.includes(name)),
      'a schedule component still defined here is a sub-domain that never left').toEqual([]);
  });

  test('window.Components carries none of them either', async ({ page }) => {
    await page.goto('/#/ops/oncall');
    await waitForAppReady(page);

    const present = await page.evaluate((names: string[]) => {
      const components = (window as any).Components || {};
      return names.filter(name => typeof components[name] !== 'undefined');
    }, MOVED);

    expect(present, 'a bridge in the global is a boundary that only looks real').toEqual([]);
  });
});
