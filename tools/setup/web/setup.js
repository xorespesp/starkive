// The starkive setup page.
// It fetches the lines and step state kept by the setup tool (`webUI` in `tools/setup`) at short intervals, and draws them as a vertical step list.
// When a prompt comes, it shows an input box and sends the answer.
// It expands only the step in progress and collapses finished steps.
// When the setup ends, it shows a result card below the step list.
// The tool does all the step work.
// This page only shows things and sends answers.
//
// Security: all text from the tool goes in only through `textContent` (backtick parts in sentences also go in as text of `code` elements).
// Links are made only for `https://` addresses and addresses of this server.

const POLL_MS = 700; // Interval between progress requests (milliseconds)
const LOST_CONTACT = 'Lost contact with the setup tool. If you closed it, run it again.';
const SLOW_WAIT_MS = 5 * 60 * 1000; // How long a waiting prompt can wait before the page says it is taking longer than usual (milliseconds)

let shown = 0; // Number of lines already drawn. Sent as `since` in the next request
let promptId = 0; // Number of the prompt shown now (input box or waiting prompt). 0 if none
let answeredId = 0; // Number of the last prompt that was finished (answer sent, or switched to typing). Even if a request sent before returns the old prompt, it isn't shown again
let finished = false; // Whether the setup ended
let waitingShown = false; // Whether the current prompt is a waiting prompt
let waitingSince = 0; // When the waiting sentence of the current prompt started to show (`Date.now()`). 0 if it isn't shown
let openNotClicked = false; // Whether the last open button (`open`) drawn in the current step is not clicked yet. The person still has work to do, so the progress indicator is hidden
const steps = []; // Page elements for each step: { item, header, marker, toggle, body, content, lines }. The order is the step number (from 1)
const expandedByUser = new Map(); // Step number -> expanded or not, as set by the person clicking the header. If missing, it follows the state
let lineTarget = null; // The line list of the step where the next line goes. Changes when the tool starts a step (`step` line)
let lastState = null; // The last progress received
let controlsStep = 0; // Number of the step that holds the prompt and progress indicator (`#controls`)

/**
 * Finds a page element by ID.
 *
 * @param {string} id The element's `id`
 * @returns {HTMLElement}
 */
const $ = (id) => document.getElementById(id);

/**
 * Creates a DOM element from a tag, properties and children.
 * String children go in as text nodes, so they aren't parsed as HTML.
 *
 * @param {string} tag Tag name
 * @param {object} [props] Properties put on the element as is
 * @param {...(Node|string)} children Children
 * @returns {HTMLElement}
 */
function el(tag, props = {}, ...children) {
  const node = Object.assign(document.createElement(tag), props);
  node.append(...children);
  return node;
}

/**
 * Splits a sentence into a list of nodes: backtick parts become `code` elements, and the rest stays text.
 *
 * @param {string} text A sentence from the tool. Parts in backticks (`...`) are code or names
 * @returns {(Node|string)[]}
 */
function richText(text) {
  return text.split('`').map((part, i) => (i % 2 === 1 ? el('code', { textContent: part }) : part));
}

/**
 * Tells whether an address may be made into a link.
 * Only `https://` addresses such as GitHub and Vercel, and addresses of this server, become links.
 *
 * @param {string} target The address
 * @returns {boolean}
 */
function isLinkable(target) {
  return target.startsWith('https://') || target.startsWith(`${location.origin}/`);
}

/**
 * Creates a link with the label `label`.
 * If the address can't be a link (`isLinkable`), it shows the label and the address as text instead.
 *
 * @param {string} label The label
 * @param {string} target The address
 * @param {string} className Link style
 * @returns {HTMLAnchorElement | HTMLParagraphElement}
 */
function newLinkOrText(label, target, className) {
  if (!isLinkable(target)) return el('p', { textContent: `${label}: ${target}` });
  return el('a', { className, href: target, target: '_blank', rel: 'noopener noreferrer', textContent: label });
}

/**
 * Creates a small button that puts the text `text` on the clipboard.
 * After copying, it shows "Copied" for a moment, or "Copy failed" if the browser refused.
 *
 * @param {string} label Button text
 * @param {string} text Text to copy
 * @param {string} className Button style
 * @returns {HTMLButtonElement}
 */
function copyButton(label, text, className) {
  const button = el('button', { type: 'button', className, textContent: label });
  button.onclick = async () => {
    try {
      await navigator.clipboard.writeText(text);
      button.textContent = 'Copied';
    } catch {
      button.textContent = 'Copy failed'; // The browser refused clipboard access
    }
    setTimeout(() => (button.textContent = label), 1500);
  };
  return button;
}

/**
 * Creates the vertical step list once, from the step names `titles`.
 * Each step has a header that expands and collapses it when clicked, and a body that holds the lines of that step.
 *
 * @param {string[]} titles Step names (`setupSteps`)
 *
 * NOTE: The list is created only once, and after that only its state changes.
 * If it were created again for every progress update, the drawn lines, the expanded state and the text in the input box would be lost.
 */
function buildSteps(titles) {
  titles.forEach((title, i) => {
    const n = i + 1;
    const marker = el('span', { className: 'step-marker' });
    const toggle = el('span', { className: 'step-toggle' });
    const header = el('button', { type: 'button', className: 'step-header' }, marker, el('span', { textContent: title }), toggle);
    const lines = el('div');
    const content = el('div', { className: 'step-content' }, lines);
    const body = el('div', { className: 'step-body' }, content);
    const item = el('li', {}, header, body);
    header.onclick = () => {
      expandedByUser.set(n, body.classList.contains('collapsed'));
      renderSteps(lastState);
    };
    const step = { item, header, marker, toggle, body, content, lines };
    steps.push(step);
    new ResizeObserver(() => updateStepHeight(step)).observe(content);
    $('steps').append(item);
  });
  lineTarget = steps[0].lines;
}

/**
 * Sets the body height of `step`: 0 if collapsed, otherwise the height of its content.
 * A fixed height can be animated, but `height: auto` can't.
 */
function updateStepHeight(step) {
  step.body.style.height = `${step.body.classList.contains('collapsed') ? 0 : step.content.offsetHeight}px`;
}

/**
 * Turns one line from the tool into a page element.
 * A step start line (`step`) isn't drawn. It makes the next lines go into the body of that step.
 *
 * @param {{ kind: string, text: string, label: string, step: number }} line The line. `kind` is one of `step`, `say`, `note`, `value`, `link`, `open`, `answer`
 * @returns {HTMLElement | null} The element to draw. `null` for a line that isn't drawn
 *
 * NOTE: Links (`link`, `open`) show only the label instead of the long address.
 * A "Copy link" button sits next to them for when the address is needed.
 * When an open button (`open`) is drawn, the progress indicator is hidden until it is clicked (`openNotClicked`).
 * The same decision must come out when a reloaded page draws the lines again.
 * So if an answer line comes after it, or the next step started, its task counts as done.
 * The copy button of a value to copy (`value`) copies only the value, without the name (the name field on the other side is already filled in).
 */
function renderLine(line) {
  switch (line.kind) {
    case 'step':
      lineTarget = steps[line.step - 1].lines;
      openNotClicked = false; // The tool moved to the next step, so the task of the open button in the earlier step is done
      return null;
    case 'note':
      return el('div', { className: 'callout' }, el('p', { className: 'callout-title', textContent: 'Note' }), el('p', {}, ...richText(line.text)));
    case 'value':
      return el(
        'div',
        { className: 'value' },
        el('span', { className: 'name', textContent: line.label }),
        el('code', { className: 'value-text', textContent: line.text }),
        copyButton('Copy', line.text, ''),
      );
    case 'link':
    case 'open': {
      const link = newLinkOrText(line.label, line.text, line.kind === 'open' ? 'primary' : '');
      if (!(link instanceof HTMLAnchorElement)) return link;
      const row = el('div', { className: 'link-row' }, link, copyButton('Copy link', line.text, 'text-button'));
      if (line.kind === 'open') {
        openNotClicked = true;
        const onPress = (event) => {
          if (event.target === link) link.classList.add('opened'); // A button opened once turns gray, so the person can see how far they got. It can be clicked again
          if (event.target.closest('a, button')) markOpenClicked();
        };
        row.onclick = onPress;
        row.onauxclick = onPress; // Opened in a new tab with the middle button
      }
      return row;
    }
    case 'answer':
      openNotClicked = false; // The person answered, so the task of the open button before it is done
      return el('p', { className: 'answer' }, ...richText(line.text), ' ', el('code', { textContent: line.label }));
    default:
      return el('p', {}, ...richText(line.text));
  }
}

/**
 * Updates the step list to match the progress `state`.
 * It sets each step's status (done, in progress, failed, pending) and marker.
 * The step in progress is always expanded, a failed step is expanded, and finished steps are collapsed.
 * (For finished or failed steps, it follows the person's choice if they clicked the header.)
 * It moves the prompt and the progress indicator to the end of the body of the step in progress.
 * When a new step starts, it scrolls to that step.
 *
 * @param {{ steps: string[], current: number, finished: boolean, failed: boolean }} state The progress
 */
function renderSteps(state) {
  const stateOf = (n) => {
    if (state.finished && !state.failed) return 'done';
    if (n < state.current) return 'done';
    if (n > state.current) return 'pending';
    return state.failed ? 'failed' : 'current';
  };
  steps.forEach((step, i) => {
    const n = i + 1;
    const status = stateOf(n);
    const expanded = status === 'current' || (status !== 'pending' && (expandedByUser.get(n) ?? status === 'failed'));
    step.item.className = status;
    step.marker.textContent = { done: '✓', failed: '✕' }[status] ?? String(n);
    step.header.disabled = status === 'pending' || status === 'current'; // The step in progress holds the prompt, so it can't be collapsed
    step.header.setAttribute('aria-expanded', String(expanded));
    step.toggle.textContent = status === 'done' ? (expanded ? 'Hide' : 'Show') : '';
    step.body.classList.toggle('collapsed', !expanded);
    updateStepHeight(step);
  });
  const controls = $('controls');
  controls.hidden = state.finished;
  if (!state.finished && state.current > 0 && state.current !== controlsStep) {
    controlsStep = state.current;
    const step = steps[state.current - 1];
    step.content.append(controls);
    // After the expand animation, so it scrolls to the final position
    const animationMs = parseFloat(getComputedStyle(step.body).transitionDuration) * 1000;
    setTimeout(() => step.item.scrollIntoView({ behavior: 'smooth', block: 'nearest' }), animationMs);
  }
}

/**
 * Shows the result card below the step list when the setup ends, and scrolls to it.
 * On success, it shows the last message and the next link to open.
 * On failure, it tells the person to fix the problem and run the tool again.
 *
 * @param {{ failed: boolean, result: { text: string, label: string, target: string } | null }} state The finished progress
 */
function showResult(state) {
  const card = $('result');
  card.classList.toggle('failed', state.failed);
  const action = $('result-action');
  action.replaceChildren();
  if (state.failed) {
    $('result-title').textContent = 'Setup stopped';
    $('result-text').textContent = 'Fix the problem shown in the step above, then run the setup tool again. Steps that are already done are skipped.';
  } else {
    $('result-title').textContent = 'Setup complete';
    $('result-text').replaceChildren(...richText(state.result ? state.result.text : 'Everything is set up.'), ' You can close this page.');
    if (state.result) {
      action.append(newLinkOrText(state.result.label, state.result.target, 'primary'));
    }
  }
  card.hidden = false;
  card.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
}

/**
 * Updates the page to match the prompt `prompt`.
 * A normal prompt is shown as an input box.
 * A waiting prompt (has `waiting`) is shown as a waiting sentence and a link to enter it by hand.
 * For the same prompt, it doesn't clear what the person was typing.
 * A prompt that was already answered or switched to typing isn't shown.
 *
 * @param {{ id: number, text: string, hint: string, secret: boolean, waiting: string, manual_entry: string } | null} prompt The prompt waiting for an answer. `null` if none
 *
 * NOTE: The input box of a secret prompt is a text box, and CSS hides its characters (`.masked`).
 * The browser doesn't see it as a sign-in form, so it doesn't offer to save a password (measured).
 * So the secret doesn't stay in the browser or its sync account.
 * The marks that keep password manager extensions from filling or saving it (`data-1p-ignore`, `data-lpignore`), and spell check off, are on the input box in the HTML.
 */
function updatePrompt(prompt) {
  const form = $('prompt');
  if (!prompt || prompt.id <= answeredId) {
    form.hidden = true;
    waitingShown = false;
    promptId = 0;
    return;
  }
  if (prompt.id === promptId) return;
  promptId = prompt.id;
  waitingSince = 0;
  if (prompt.waiting) {
    $('waiting-text').replaceChildren(...richText(prompt.waiting));
    $('switch-to-typing').textContent = prompt.manual_entry;
    form.hidden = true;
    waitingShown = true;
    return;
  }
  $('prompt-label').replaceChildren(...richText(prompt.text));
  const input = $('prompt-input');
  input.classList.toggle('masked', prompt.secret);
  input.placeholder = prompt.hint;
  input.value = '';
  $('prompt-error').hidden = true;
  waitingShown = false;
  form.hidden = false;
  input.focus();
}

/**
 * Updates the progress indicators (tool is working, waiting prompt) to match the current state.
 * Hides them if the setup ended, an input box is shown, or there is an open button not clicked yet.
 *
 * NOTE: While an open button isn't clicked, a waiting prompt shows only its link to enter the value by hand.
 * A moving indicator would make the person hesitate to click the button, so the indicator and the waiting sentence are hidden.
 * The link stays, because the person may not need that button at all.
 * If the waiting sentence shows for longer than `SLOW_WAIT_MS`, a small note says so.
 * The wait has no time limit, and some failures don't go away by waiting (for example, the value was pasted wrong).
 * The page can't tell why, so the note only points to the steps above.
 * The link below it is the way out.
 */
function updateIndicators() {
  const lineShown = waitingShown && !openNotClicked;
  if (!lineShown) waitingSince = 0;
  else if (!waitingSince) waitingSince = Date.now();
  $('waiting').hidden = !waitingShown;
  $('waiting').classList.toggle('quiet', openNotClicked);
  $('waiting-line').hidden = openNotClicked;
  $('waiting-slow').hidden = !lineShown || Date.now() - waitingSince < SLOW_WAIT_MS;
  $('busy').hidden = finished || !$('prompt').hidden || waitingShown || openNotClicked;
}

/**
 * Marks the open button as clicked and updates the progress indicators again.
 */
function markOpenClicked() {
  openNotClicked = false;
  updateIndicators();
}

/**
 * Fetches the progress once and draws it, and schedules the next request if the setup hasn't ended.
 *
 * NOTE: If the tool is stopped, the request fails.
 * On failure, it only says that the connection was lost, and asks again at the same interval.
 * After a reload, it receives the lines again from the first line.
 */
async function poll() {
  try {
    const res = await fetch(`/state?since=${shown}`, { cache: 'no-store' });
    if (!res.ok) throw new Error(`${res.status}`);
    const state = await res.json();
    if (steps.length === 0) buildSteps(state.steps);
    $('version').textContent = state.version;
    for (const line of state.lines) {
      const node = renderLine(line);
      if (node) lineTarget.append(node);
    }
    shown = state.total;
    lastState = state;
    finished = state.finished;
    renderSteps(state);
    $('status').textContent = '';
    if (finished) {
      updatePrompt(null);
      updateIndicators();
      showResult(state);
      return;
    }
    updatePrompt(state.prompt);
    updateIndicators();
  } catch {
    $('busy').hidden = true;
    $('status').textContent = LOST_CONTACT;
  }
  setTimeout(poll, POLL_MS);
}

/**
 * Sends the body `body` with a prompt number to the tool's `path`.
 * A 409 (the prompt already ended) counts as success, because the next progress update fixes the page.
 *
 * @param {string} path The path to send to
 * @param {{ id: number }} body The body to send
 */
async function sendToTool(path, body) {
  const res = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
  if (!res.ok && res.status !== 409) throw new Error(await res.text());
}

/**
 * Marks the prompt `id` as finished and removes it from the page, if it is still the one shown.
 * The next prompt comes with the progress.
 *
 * @param {number} id Number of the prompt the request was sent for
 *
 * NOTE: The tool shows the next prompt as soon as it gets the answer.
 * So the progress can bring the next prompt before the response to this request arrives.
 * Judging by the prompt shown at that moment would hide that next prompt for good.
 */
function dismissPrompt(id) {
  answeredId = Math.max(answeredId, id);
  if (promptId !== id) return; // A later prompt is already shown
  promptId = 0;
  waitingShown = false;
  openNotClicked = false;
  $('prompt').hidden = true;
  updateIndicators();
}

/**
 * Sends the answer in the input box to the tool.
 * If it can't be sent, it shows the reason below the input box.
 *
 * @param {SubmitEvent} event The form submit event. It is prevented, so the page doesn't navigate
 */
async function submitAnswer(event) {
  event.preventDefault();
  const error = $('prompt-error');
  const id = promptId;
  try {
    await sendToTool('/answer', { id, value: $('prompt-input').value });
    dismissPrompt(id);
  } catch (err) {
    if (promptId !== id) return; // A later prompt is already shown, so the error isn't about it
    // `fetch` throws a `TypeError` when the request doesn't reach the tool
    error.textContent = err instanceof TypeError ? "Couldn't send your answer to the setup tool. If you closed it, run it again." : `Couldn't send the answer: ${err.message}`;
    error.hidden = false;
  }
}

/**
 * Tells the tool, from a waiting prompt, that the person will enter it by hand.
 * The tool stops the background check and shows a new input box prompt.
 */
async function switchToTyping() {
  const id = promptId;
  try {
    await sendToTool('/switch-to-typing', { id });
    dismissPrompt(id);
  } catch (err) {
    $('status').textContent = err instanceof TypeError ? LOST_CONTACT : `Couldn't switch to typing: ${err.message}`;
  }
}

$('prompt').onsubmit = submitAnswer;
$('switch-to-typing').onclick = switchToTyping;
poll();
