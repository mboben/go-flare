import { networkIDs, pvm, utils } from "@flarenetwork/flarejs";
import { issuePChainTx, localFlareContext } from "./utils";
import { runTest } from "./runner";

const blsPublicKey = '0x917386c374aab0ea3d2bda96768f2be1f0b11483cd5c41bd9cddd3892b693ab84efecc6cf70300d614dcdc6d298ab659'; 
const blsSignature = '0x81c7174fc3bc9bbf00a1e26cc7b177d92589b517df5c46844498c14ede655bfedb89929e3d2974210983a1ff06a2a39b05208974a2d3dddac4948bcc81f367717d960be9704b775f21022d639b439135d34b7fb1f2bacbb5ffab3ddafc86220f'

async function addValidator(nodeID: string, endTime: number, weight: number) {
    const ctx = await localFlareContext();

    // Create the transaction to add a validator
    console.log(`Creating add validator transaction for node ${nodeID} with weight ${weight}...`);

    const { utxos } = await ctx.pvmapi.getUTXOs({
        addresses: [ctx.addressP]
    });
    const tx = pvm.newAddPermissionlessValidatorTx(
        ctx.context,
        utxos,
        [utils.bech32ToBytes(ctx.addressP)],
        nodeID,
        networkIDs.PrimaryNetworkID.toString(),
        BigInt(Date.now()) / 1000n,
        BigInt(endTime),
        BigInt(weight * 1e9), 
        [utils.bech32ToBytes(ctx.addressP)],
        [utils.bech32ToBytes(ctx.addressP)],
        10_0000,
        undefined,
        1,
        0n,
        utils.hexToBuffer(blsPublicKey),
        utils.hexToBuffer(blsSignature)
    );

    await issuePChainTx(ctx.pvmapi, tx, ctx.privateKey);
}

runTest(() => addValidator(
    'NodeID-MFrZFVCXPv5iCn6M9K6XduxGTYp891xXZ',
    Math.ceil(Date.now() / 1000) + 14 * 24 * 60 * 60 + 5, // 14 days (+ 5 seconds) from now 
    10_000
))